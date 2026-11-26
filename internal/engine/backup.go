package engine

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/phantomwar/ad-postgres-backup/internal/config"
	"github.com/phantomwar/ad-postgres-backup/internal/crypto"
	"github.com/phantomwar/ad-postgres-backup/internal/rotation"
	"github.com/phantomwar/ad-postgres-backup/internal/s3sync"
	"github.com/phantomwar/ad-postgres-backup/internal/server"
)

// CommandRunner abstracts external CLI execution (pg_dump, pg_dumpall, pg_restore, psql, initdb, pg_ctl).
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, env []string, stdin io.Reader, stdout io.Writer) error
}

// ExecRunner executes commands on the host OS.
type ExecRunner struct{}

// Run executes name with args, additional env vars, optional stdin/stdout, capturing stderr on failure.
func (ExecRunner) Run(ctx context.Context, name string, args []string, env []string, stdin io.Reader, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = stdin
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		var outBuf strings.Builder
		cmd.Stdout = &outBuf
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(errBuf.String())
		if errMsg != "" {
			return fmt.Errorf("%s %v: %w (%s)", name, args, err, errMsg)
		}
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// RunCycle executes one complete backup cycle across all configured databases and optional STORAGE_DIR.
func RunCycle(ctx context.Context, logger *slog.Logger, cfg *config.Config, runner CommandRunner, s3Client *s3sync.Client) ([]server.ArtifactSummary, error) {
	if err := cfg.ValidateForBackup(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.BackupDir, "last"), 0o755); err != nil {
		return nil, fmt.Errorf("prepare backup dir: %w", err)
	}
	if err := cfg.ValidateDirWritable(); err != nil {
		return nil, err
	}

	if err := notifyHooks(ctx, logger, cfg, "pre-backup", ""); err != nil {
		_ = notifyHooks(ctx, logger, cfg, "error", err.Error())
		return nil, err
	}

	now := time.Now()
	keep := rotation.KeepPolicy{
		Mins:   cfg.KeepMins,
		Days:   cfg.KeepDays,
		Weeks:  cfg.KeepWeeks,
		Months: cfg.KeepMonths,
	}

	if cfg.PruneBeforeDump {
		ext := effectiveExt(cfg.BackupSuffix, cfg.EncryptionEnabled())
		for _, db := range cfg.PostgresDBs {
			_, _ = rotation.Prune(cfg.BackupDir, db, ext, now, keep)
		}
		if cfg.StorageDir != "" {
			_, _ = rotation.Prune(cfg.BackupDir, cfg.StoragePrefix, effectiveExt(".tar.gz", cfg.EncryptionEnabled()), now, keep)
		}
	}

	if err := CheckDiskUsage(cfg.BackupDir, cfg.DiskMaxUsagePct); err != nil {
		_ = notifyHooks(ctx, logger, cfg, "error", err.Error())
		return nil, err
	}

	pgEnv := buildPGEnv(cfg)
	var summaries []server.ArtifactSummary

	for _, db := range cfg.PostgresDBs {
		sum, err := backupSingleDatabase(ctx, logger, cfg, runner, pgEnv, s3Client, db, keep, now)
		if err != nil {
			_ = notifyHooks(ctx, logger, cfg, "error", err.Error())
			return summaries, err
		}
		summaries = append(summaries, sum)
	}

	if cfg.StorageDir != "" {
		if _, err := os.Stat(cfg.StorageDir); err == nil {
			sum, err := backupStorageDir(ctx, logger, cfg, s3Client, keep, now)
			if err != nil {
				_ = notifyHooks(ctx, logger, cfg, "error", err.Error())
				return summaries, err
			}
			summaries = append(summaries, sum)
		} else {
			logger.Warn("STORAGE_DIR configured but missing, skipping directory archive", "dir", cfg.StorageDir)
		}
	}

	if s3Client != nil {
		deletedRemote, err := s3Client.PruneExpired(ctx, cfg.S3RetentionDays, now)
		if err != nil {
			_ = notifyHooks(ctx, logger, cfg, "error", err.Error())
			return summaries, err
		}
		if deletedRemote > 0 {
			logger.Info("pruned expired remote S3 objects", "count", deletedRemote)
		}
	}

	if err := notifyHooks(ctx, logger, cfg, "post-backup", fmt.Sprintf("%d artifact(s) backed up", len(summaries))); err != nil {
		return summaries, err
	}
	return summaries, nil
}

func backupSingleDatabase(
	ctx context.Context,
	logger *slog.Logger,
	cfg *config.Config,
	runner CommandRunner,
	pgEnv []string,
	s3Client *s3sync.Client,
	db string,
	keep rotation.KeepPolicy,
	now time.Time,
) (server.ArtifactSummary, error) {
	lastDir := filepath.Join(cfg.BackupDir, "last")
	tmpRaw := filepath.Join(lastDir, fmt.Sprintf(".tmp-%s-%s%s", db, now.Format("20060102-150405"), cfg.BackupSuffix))
	defer func() {
		_ = os.RemoveAll(tmpRaw)
	}()

	logger.Info("starting postgres dump", "database", db, "cluster", cfg.PostgresCluster)
	if cfg.PostgresCluster {
		if err := runClusterDump(ctx, runner, pgEnv, db, cfg.PostgresExtraOpts, tmpRaw); err != nil {
			return server.ArtifactSummary{}, fmt.Errorf("cluster dump %q: %w", db, err)
		}
	} else {
		args := []string{"-h", cfg.PostgresHost, "-p", strconv.Itoa(cfg.PostgresPort), "-U", cfg.PostgresUser, "-d", db, "-f", tmpRaw}
		args = append(args, cfg.PostgresExtraOpts...)
		if err := runner.Run(ctx, "pg_dump", args, pgEnv, nil, nil); err != nil {
			return server.ArtifactSummary{}, fmt.Errorf("pg_dump %q: %w", db, err)
		}
	}

	if err := validateDumpArtifact(ctx, runner, tmpRaw, cfg.BackupSuffix); err != nil {
		return server.ArtifactSummary{}, fmt.Errorf("validate dump %q: %w", db, err)
	}

	return finalizeAndDistribute(ctx, logger, cfg, s3Client, db, tmpRaw, cfg.BackupSuffix, keep, now)
}

func backupStorageDir(
	ctx context.Context,
	logger *slog.Logger,
	cfg *config.Config,
	s3Client *s3sync.Client,
	keep rotation.KeepPolicy,
	now time.Time,
) (server.ArtifactSummary, error) {
	lastDir := filepath.Join(cfg.BackupDir, "last")
	suffix := ".tar.gz"
	tmpTar := filepath.Join(lastDir, fmt.Sprintf(".tmp-%s-%s%s", cfg.StoragePrefix, now.Format("20060102-150405"), suffix))
	defer func() {
		_ = os.RemoveAll(tmpTar)
	}()

	logger.Info("archiving storage directory", "dir", cfg.StorageDir)
	if err := ArchiveDirectory(cfg.StorageDir, tmpTar); err != nil {
		return server.ArtifactSummary{}, err
	}
	if err := verifyGzipFile(tmpTar); err != nil {
		return server.ArtifactSummary{}, fmt.Errorf("verify storage archive gzip: %w", err)
	}
	return finalizeAndDistribute(ctx, logger, cfg, s3Client, cfg.StoragePrefix, tmpTar, suffix, keep, now)
}

func finalizeAndDistribute(
	ctx context.Context,
	logger *slog.Logger,
	cfg *config.Config,
	s3Client *s3sync.Client,
	name string,
	tmpPlainPath string,
	plainExt string,
	keep rotation.KeepPolicy,
	now time.Time,
) (server.ArtifactSummary, error) {
	workPath := tmpPlainPath
	finalExt := effectiveExt(plainExt, cfg.EncryptionEnabled())

	if cfg.EncryptionEnabled() {
		info, err := os.Stat(tmpPlainPath)
		if err != nil {
			return server.ArtifactSummary{}, err
		}
		if info.IsDir() {
			return server.ArtifactSummary{}, fmt.Errorf("age encryption is incompatible with directory format backups (%s)", tmpPlainPath)
		}
		tmpEnc := tmpPlainPath + ".age"
		defer func() {
			_ = os.Remove(tmpEnc)
		}()
		err = crypto.EncryptFile(tmpPlainPath, tmpEnc, crypto.EncryptionConfig{
			Recipients: cfg.AgeRecipients,
			Passphrase: cfg.BackupPass,
		})
		if err != nil {
			return server.ArtifactSummary{}, fmt.Errorf("encrypt %q: %w", name, err)
		}
		_ = os.Remove(tmpPlainPath)
		workPath = tmpEnc
	}

	var tmpSumPath string
	if cfg.BackupChecksum {
		info, err := os.Stat(workPath)
		if err == nil && !info.IsDir() {
			sumPath, err := crypto.WriteSHA256File(workPath)
			if err != nil {
				return server.ArtifactSummary{}, err
			}
			tmpSumPath = sumPath
			defer func() {
				_ = os.Remove(tmpSumPath)
			}()
		}
	}

	finalPath, err := rotation.RotateGFS(cfg.BackupDir, name, finalExt, workPath, tmpSumPath, cfg.BackupLatestType, now)
	if err != nil {
		return server.ArtifactSummary{}, fmt.Errorf("rotate %q: %w", name, err)
	}

	pruned, err := rotation.Prune(cfg.BackupDir, name, finalExt, now, keep)
	if err != nil {
		return server.ArtifactSummary{}, fmt.Errorf("prune %q: %w", name, err)
	}
	if pruned > 0 {
		logger.Info("pruned expired local backups", "name", name, "count", pruned)
	}

	var sizeBytes int64
	if st, err := os.Stat(finalPath); err == nil {
		sizeBytes = st.Size()
	}

	s3Synced := false
	if s3Client != nil {
		if err := s3Client.UploadBackup(ctx, finalPath); err != nil {
			return server.ArtifactSummary{}, fmt.Errorf("s3 upload %q: %w", name, err)
		}
		s3Synced = true
	}

	logger.Info("backup completed", "name", name, "path", finalPath, "size_bytes", sizeBytes, "encrypted", cfg.EncryptionEnabled(), "s3_synced", s3Synced)
	return server.ArtifactSummary{
		Name:      name,
		Path:      finalPath,
		SizeBytes: sizeBytes,
		Encrypted: cfg.EncryptionEnabled(),
		S3Synced:  s3Synced,
	}, nil
}

func runClusterDump(ctx context.Context, runner CommandRunner, pgEnv []string, db string, extraOpts []string, dstPath string) error {
	f, err := os.OpenFile(dstPath+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(dstPath + ".part")
		}
	}()

	gz := gzip.NewWriter(f)
	args := []string{"-l", db}
	args = append(args, extraOpts...)
	if err := runner.Run(ctx, "pg_dumpall", args, pgEnv, nil, gz); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(dstPath+".part", dstPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func validateDumpArtifact(ctx context.Context, runner CommandRunner, path, suffix string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return runner.Run(ctx, "pg_restore", []string{"--list", path}, nil, nil, io.Discard)
	}
	if info.Size() == 0 {
		return fmt.Errorf("dump file %q is empty (0 bytes)", path)
	}
	lower := strings.ToLower(suffix)
	if strings.HasSuffix(lower, ".gz") {
		return verifyGzipFile(path)
	}
	if strings.HasSuffix(lower, ".dump") {
		return runner.Run(ctx, "pg_restore", []string{"--list", path}, nil, nil, io.Discard)
	}
	return nil
}

func verifyGzipFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("invalid gzip header: %w", err)
	}
	defer gz.Close()
	n, err := io.Copy(io.Discard, gz)
	if err != nil {
		return fmt.Errorf("corrupt gzip stream: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("empty decompressed dump stream")
	}
	return nil
}

func buildPGEnv(cfg *config.Config) []string {
	env := []string{
		"PGHOST=" + cfg.PostgresHost,
		"PGPORT=" + strconv.Itoa(cfg.PostgresPort),
		"PGUSER=" + cfg.PostgresUser,
	}
	if cfg.PostgresPassword != "" {
		env = append(env, "PGPASSWORD="+cfg.PostgresPassword)
	}
	if cfg.PostgresPassfileStore != "" {
		env = append(env, "PGPASSFILE="+cfg.PostgresPassfileStore)
	}
	return env
}

func effectiveExt(suffix string, encrypted bool) string {
	if encrypted {
		return suffix + ".age"
	}
	return suffix
}
