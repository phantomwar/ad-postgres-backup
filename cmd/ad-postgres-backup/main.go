package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/phantomwar/ad-postgres-backup/internal/config"
	"github.com/phantomwar/ad-postgres-backup/internal/crypto"
	"github.com/phantomwar/ad-postgres-backup/internal/engine"
	"github.com/phantomwar/ad-postgres-backup/internal/s3sync"
	"github.com/phantomwar/ad-postgres-backup/internal/scheduler"
	"github.com/phantomwar/ad-postgres-backup/internal/server"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd := "daemon"
	args := os.Args[1:]
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	var err error
	switch cmd {
	case "version", "-v", "--version":
		fmt.Printf("ad-postgres-backup %s\n", version)
		return
	case "keygen":
		err = cmdKeygen()
	case "healthcheck":
		err = cmdHealthcheck()
	case "backup":
		err = cmdBackup(logger)
	case "restore-test":
		err = cmdRestoreTest(logger)
	case "decrypt":
		err = cmdDecrypt(args)
	case "daemon":
		err = cmdDaemon(logger)
	case "help", "-h", "--help":
		printUsage()
		return
	default:
		err = fmt.Errorf("unknown subcommand %q (expected: daemon, backup, restore-test, decrypt, keygen, healthcheck, version)", cmd)
	}

	if err != nil {
		logger.Error("command failed", "command", cmd, "error", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`ad-postgres-backup — PostgreSQL GFS local + S3 + age encrypted backup tool

Usage:
  ad-postgres-backup [command]

Commands:
  daemon         Run scheduled backups + HTTP /healthz & /metrics server (default)
  backup         Execute one backup cycle immediately and exit
  restore-test   Verify checksum, decrypt, restore latest DB dump into ephemeral cluster, and query
  decrypt <in> <out>  Decrypt a .age backup file using BACKUP_AGE_IDENTITY or BACKUP_PASSWORD
  keygen         Generate a fresh X25519 age recipient/identity keypair
  healthcheck    Probe local /healthz endpoint (for Docker HEALTHCHECK)
  version        Print version
`)
}

func cmdKeygen() error {
	pub, sec, err := crypto.GenerateKeypair()
	if err != nil {
		return err
	}
	fmt.Printf("# Store the secret identity strictly inside your secrets vault:\n")
	fmt.Printf("BACKUP_AGE_RECIPIENTS=%s\n", pub)
	fmt.Printf("BACKUP_AGE_IDENTITY=%s\n", sec)
	return nil
}

func cmdHealthcheck() error {
	cfg, err := config.LoadFromOS()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 4 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.HealthcheckPort)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func cmdBackup(logger *slog.Logger) error {
	ctx := context.Background()
	cfg, err := config.LoadFromOS()
	if err != nil {
		return err
	}
	s3Client, err := buildS3Client(ctx, cfg)
	if err != nil {
		return err
	}
	_, err = engine.RunCycle(ctx, logger, cfg, engine.ExecRunner{}, s3Client)
	return err
}

func cmdRestoreTest(logger *slog.Logger) error {
	cfg, err := config.LoadFromOS()
	if err != nil {
		return err
	}
	res, err := engine.RunRestoreTest(context.Background(), logger, cfg, engine.ExecRunner{})
	if err != nil {
		return err
	}
	fmt.Printf("restore-test passed: query_result=%s\n", res)
	return nil
}

func cmdDecrypt(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: ad-postgres-backup decrypt <input.age> <output>")
	}
	cfg, err := config.LoadFromOS()
	if err != nil {
		return err
	}
	return crypto.DecryptFile(args[0], args[1], crypto.DecryptionConfig{
		Identity:   cfg.AgeIdentity,
		Passphrase: cfg.BackupPass,
	})
}

func cmdDaemon(logger *slog.Logger) error {
	cfg, err := config.LoadFromOS()
	if err != nil {
		return err
	}
	sched, err := scheduler.Parse(cfg.Schedule)
	if err != nil {
		return fmt.Errorf("parse SCHEDULE %q: %w", cfg.Schedule, err)
	}
	if cfg.ValidateOnStart {
		if err := cfg.ValidateForBackup(); err != nil {
			return fmt.Errorf("startup validation failed: %w", err)
		}
		if err := os.MkdirAll(cfg.BackupDir, 0o750); err != nil {
			return err
		}
		if err := cfg.ValidateDirWritable(); err != nil {
			return fmt.Errorf("startup directory check failed: %w", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s3Client, err := buildS3Client(ctx, cfg)
	if err != nil {
		return err
	}

	tracker := server.NewTracker(time.Now())
	maxStale := time.Duration(cfg.BackupMaxStaleHours) * time.Hour
	httpSrv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.HealthcheckPort),
		Handler:           server.Handler(tracker, maxStale),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("healthcheck HTTP server listening", "port", cfg.HealthcheckPort, "schedule", cfg.Schedule)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("healthcheck server error", "error", err)
			stop()
		}
	}()

	runAndTrack := func() {
		start := time.Now()
		artifacts, err := engine.RunCycle(ctx, logger, cfg, engine.ExecRunner{}, s3Client)
		dur := time.Since(start)
		if err != nil {
			logger.Error("scheduled backup failed", "duration", dur, "error", err)
			tracker.RecordFailure(time.Now(), dur, err)
			return
		}
		tracker.RecordSuccess(time.Now(), dur, artifacts)
	}

	if cfg.BackupOnStart {
		runAndTrack()
	}

	for {
		next := sched.Next(time.Now())
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		logger.Info("waiting for next scheduled backup", "next_run", next.Format(time.RFC3339), "in", wait.Round(time.Second))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutCtx)
			logger.Info("daemon stopped gracefully")
			return nil
		case <-timer.C:
			runAndTrack()
		}
	}
}

func buildS3Client(ctx context.Context, cfg *config.Config) (*s3sync.Client, error) {
	if cfg.S3Bucket == "" {
		return nil, nil
	}
	return s3sync.NewClient(ctx, s3sync.Options{
		Endpoint:          cfg.S3Endpoint,
		Bucket:            cfg.S3Bucket,
		Region:            cfg.S3Region,
		Prefix:            cfg.S3Prefix,
		AccessKeyID:       cfg.S3AccessKeyID,
		SecretAccessKey:   cfg.S3SecretAccessKey,
		ForcePathStyle:    cfg.S3ForcePathStyle,
		RequireEncryption: cfg.S3RequireEncryption,
	})
}
