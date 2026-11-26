package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/phantomwar/ad-postgres-backup/internal/config"
	"github.com/phantomwar/ad-postgres-backup/internal/crypto"
	"github.com/phantomwar/ad-postgres-backup/internal/rotation"
)

// RunRestoreTest locates the latest database backup in BACKUP_DIR, validates its checksum,
// decrypts it if age-encrypted, starts an ephemeral PostgreSQL instance, restores the backup,
// and executes RESTORE_TEST_QUERY to verify recoverability.
func RunRestoreTest(ctx context.Context, logger *slog.Logger, cfg *config.Config, runner CommandRunner) (string, error) {
	backupFile, err := rotation.FindLatestDatabaseBackup(cfg.BackupDir, cfg.StoragePrefix)
	if err != nil {
		return "", err
	}
	logger.Info("selected backup for restore-test", "file", backupFile)

	if err := crypto.VerifySHA256File(backupFile); err != nil {
		return "", fmt.Errorf("checksum verification failed: %w", err)
	}

	tmpWork, err := os.MkdirTemp("", "ad-pg-restore-test-*")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = os.RemoveAll(tmpWork)
	}()

	plainPath := backupFile
	if strings.HasSuffix(backupFile, ".age") {
		plainName := strings.TrimSuffix(filepath.Base(backupFile), ".age")
		plainPath = filepath.Join(tmpWork, plainName)
		logger.Info("decrypting age backup for restore verification")
		if err := crypto.DecryptFile(backupFile, plainPath, crypto.DecryptionConfig{
			Identity:   cfg.AgeIdentity,
			Passphrase: cfg.BackupPass,
		}); err != nil {
			return "", fmt.Errorf("decrypt backup: %w", err)
		}
	}

	pgData := filepath.Join(tmpWork, "pgdata")
	if err := os.MkdirAll(pgData, 0o700); err != nil {
		return "", err
	}

	useSuExec := false
	if os.Geteuid() == 0 {
		if u, err := user.Lookup("postgres"); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			_ = os.Chown(tmpWork, uid, gid)
			_ = os.Chown(pgData, uid, gid)
			_ = os.Chmod(tmpWork, 0o755)
			useSuExec = true
		}
	}

	runPG := func(bin string, args ...string) error {
		if useSuExec {
			wrapped := append([]string{"postgres", bin}, args...)
			return runner.Run(ctx, "su-exec", wrapped, nil, nil, nil)
		}
		return runner.Run(ctx, bin, args, nil, nil, nil)
	}

	portStr := strconv.Itoa(cfg.RestoreTestPort)
	logger.Info("initializing ephemeral PostgreSQL cluster", "port", portStr)
	if err := runPG("initdb", "-D", pgData, "-E", "UTF8", "--auth=trust", "-U", "postgres"); err != nil {
		return "", fmt.Errorf("initdb: %w", err)
	}
	pgOpts := fmt.Sprintf("-p %s -k %s -c listen_addresses=127.0.0.1", portStr, tmpWork)
	if err := runPG("pg_ctl", "-D", pgData, "-o", pgOpts, "-w", "start"); err != nil {
		return "", fmt.Errorf("pg_ctl start: %w", err)
	}
	defer func() {
		_ = runPG("pg_ctl", "-D", pgData, "-m", "immediate", "stop")
	}()

	const targetDB = "restore_verify"
	if err := runPG("createdb", "-h", "127.0.0.1", "-p", portStr, "-U", "postgres", targetDB); err != nil {
		return "", fmt.Errorf("createdb: %w", err)
	}

	lower := strings.ToLower(plainPath)
	switch {
	case strings.HasSuffix(lower, ".dump") || isDir(plainPath):
		if err := runPG("pg_restore", "-h", "127.0.0.1", "-p", portStr, "-U", "postgres", "-d", targetDB, "--no-owner", "--no-privileges", plainPath); err != nil {
			return "", fmt.Errorf("pg_restore: %w", err)
		}
	case strings.HasSuffix(lower, ".sql.gz") || strings.HasSuffix(lower, ".gz"):
		f, err := os.Open(plainPath)
		if err != nil {
			return "", err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return "", fmt.Errorf("open gzip stream: %w", err)
		}
		// Stream decompressed SQL directly to psql over TCP localhost.
		err = runner.Run(ctx, "psql", []string{"-h", "127.0.0.1", "-p", portStr, "-U", "postgres", "-d", targetDB, "-v", "ON_ERROR_STOP=1"}, nil, gz, nil)
		_ = gz.Close()
		_ = f.Close()
		if err != nil {
			return "", fmt.Errorf("psql gzip restore: %w", err)
		}
	default:
		f, err := os.Open(plainPath)
		if err != nil {
			return "", err
		}
		err = runner.Run(ctx, "psql", []string{"-h", "127.0.0.1", "-p", portStr, "-U", "postgres", "-d", targetDB, "-v", "ON_ERROR_STOP=1"}, nil, f, nil)
		_ = f.Close()
		if err != nil {
			return "", fmt.Errorf("psql restore: %w", err)
		}
	}

	var out bytes.Buffer
	if err := runner.Run(ctx, "psql", []string{"-h", "127.0.0.1", "-p", portStr, "-U", "postgres", "-d", targetDB, "-tAc", cfg.RestoreTestQuery}, nil, nil, &out); err != nil {
		return "", fmt.Errorf("sanity query failed: %w", err)
	}

	res := strings.TrimSpace(out.String())
	logger.Info("restore-test passed", "backup", backupFile, "query_result", res)
	return res, nil
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
