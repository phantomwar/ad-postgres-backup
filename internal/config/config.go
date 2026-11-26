package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// NoneSentinel mirrors the "**None**" placeholder used by prodrigestivill/postgres-backup-local.
const NoneSentinel = "**None**"

// Config holds all runtime settings loaded from environment variables and *_FILE paths.
type Config struct {
	// Database connection (prodrigestivill compatible)
	PostgresHost          string
	PostgresPort          int
	PostgresUser          string
	PostgresPassword      string
	PostgresPassfileStore string
	PostgresDBs           []string
	PostgresCluster       bool
	PostgresExtraOpts     []string

	// Scheduling & Healthcheck
	Schedule            string
	ValidateOnStart     bool
	BackupOnStart       bool
	HealthcheckPort     int
	BackupMaxStaleHours int

	// Local storage & GFS retention
	BackupDir        string
	BackupSuffix     string
	BackupLatestType string // "symlink", "hardlink", "none"
	KeepMins         int
	KeepDays         int
	KeepWeeks        int
	KeepMonths       int

	// Hooks & Webhooks
	HooksDir             string
	WebhookURL           string
	WebhookErrorURL      string
	WebhookPreBackupURL  string
	WebhookPostBackupURL string
	BackupChecksum       bool
	DiskMaxUsagePct      int
	PruneBeforeDump      bool

	// Encryption (age X25519 recipients or scrypt passphrase)
	AgeRecipients []string
	AgeIdentity   string
	BackupPass    string

	// Companion directory archival (e.g. uploaded media)
	StorageDir    string
	StoragePrefix string

	// Offsite S3 sync (compatible with Garage, MinIO, AWS S3, R2)
	S3Bucket            string
	S3Endpoint          string
	S3Region            string
	S3Prefix            string
	S3AccessKeyID       string
	S3SecretAccessKey   string
	S3ForcePathStyle    bool
	S3RetentionDays     int
	S3RequireEncryption bool

	// Restore verification settings
	RestoreTestQuery string
	RestoreTestPort  int
}

// EncryptionEnabled returns true if either age recipients or a symmetric passphrase is set.
func (c *Config) EncryptionEnabled() bool {
	return len(c.AgeRecipients) > 0 || c.BackupPass != ""
}

// LoadFromOS loads configuration from os.Getenv.
func LoadFromOS() (*Config, error) {
	return Load(os.Getenv, os.ReadFile)
}

// Load parses environment variables using injectable getenv and readFile functions.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (*Config, error) {
	resolve := func(key, fileKey string) (string, error) {
		val := cleanEnv(getenv(key))
		if fileKey != "" {
			if fpath := cleanEnv(getenv(fileKey)); fpath != "" {
				data, err := readFile(fpath)
				if err != nil {
					return "", fmt.Errorf("read %s (%q): %w", fileKey, fpath, err)
				}
				return strings.TrimRight(string(data), "\r\n"), nil
			}
		}
		return val, nil
	}

	var err error
	cfg := &Config{
		Schedule:             defaultStr(cleanEnv(getenv("SCHEDULE")), "@daily"),
		ValidateOnStart:      parseBool(cleanEnv(getenv("VALIDATE_ON_START")), true),
		BackupOnStart:        parseBool(cleanEnv(getenv("BACKUP_ON_START")), false),
		BackupDir:            defaultStr(cleanEnv(getenv("BACKUP_DIR")), "/backups"),
		BackupSuffix:         defaultStr(cleanEnv(getenv("BACKUP_SUFFIX")), ".sql.gz"),
		BackupLatestType:     strings.ToLower(defaultStr(cleanEnv(getenv("BACKUP_LATEST_TYPE")), "symlink")),
		HooksDir:             defaultStr(cleanEnv(getenv("HOOKS_DIR")), "/hooks"),
		WebhookURL:           cleanEnv(getenv("WEBHOOK_URL")),
		WebhookErrorURL:      cleanEnv(getenv("WEBHOOK_ERROR_URL")),
		WebhookPreBackupURL:  cleanEnv(getenv("WEBHOOK_PRE_BACKUP_URL")),
		WebhookPostBackupURL: cleanEnv(getenv("WEBHOOK_POST_BACKUP_URL")),
		PostgresCluster:      parseBool(cleanEnv(getenv("POSTGRES_CLUSTER")), false),
		BackupChecksum:       parseBool(cleanEnv(getenv("BACKUP_CHECKSUM")), true),
		PruneBeforeDump:      parseBool(cleanEnv(getenv("PRUNE_BEFORE_DUMP")), false),
		StorageDir:           cleanEnv(getenv("STORAGE_DIR")),
		StoragePrefix:        defaultStr(cleanEnv(getenv("STORAGE_PREFIX")), "storage"),
		S3Bucket:             cleanEnv(getenv("S3_BUCKET")),
		S3Endpoint:           cleanEnv(getenv("S3_ENDPOINT")),
		S3Prefix:             strings.Trim(cleanEnv(getenv("S3_PREFIX")), "/"),
		S3ForcePathStyle:     parseBool(cleanEnv(getenv("S3_FORCE_PATH_STYLE")), true),
		S3RequireEncryption:  parseBool(cleanEnv(getenv("S3_REQUIRE_ENCRYPTION")), true),
		RestoreTestQuery:     defaultStr(cleanEnv(getenv("RESTORE_TEST_QUERY")), "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'"),
	}

	// Host & Port with legacy linked-container fallback
	cfg.PostgresHost = cleanEnv(getenv("POSTGRES_HOST"))
	if cfg.PostgresHost == "" {
		cfg.PostgresHost = cleanEnv(getenv("POSTGRES_PORT_5432_TCP_ADDR"))
	}
	portRaw := cleanEnv(getenv("POSTGRES_PORT"))
	if portRaw == "" {
		portRaw = cleanEnv(getenv("POSTGRES_PORT_5432_TCP_PORT"))
	}
	if cfg.PostgresPort, err = parseIntDefault(portRaw, 5432, "POSTGRES_PORT"); err != nil {
		return nil, err
	}

	if cfg.PostgresUser, err = resolve("POSTGRES_USER", "POSTGRES_USER_FILE"); err != nil {
		return nil, err
	}
	if cfg.PostgresPassword, err = resolve("POSTGRES_PASSWORD", "POSTGRES_PASSWORD_FILE"); err != nil {
		return nil, err
	}
	cfg.PostgresPassfileStore = cleanEnv(getenv("POSTGRES_PASSFILE_STORE"))

	dbRaw, err := resolve("POSTGRES_DB", "POSTGRES_DB_FILE")
	if err != nil {
		return nil, err
	}
	cfg.PostgresDBs = splitList(dbRaw, false)

	// POSTGRES_EXTRA_OPTS: respect explicit empty string ("POSTGRES_EXTRA_OPTS=") vs unset.
	extraRaw, extraSet := getenv("POSTGRES_EXTRA_OPTS"), cleanEnv(getenv("POSTGRES_EXTRA_OPTS")) != ""
	_ = extraRaw
	if extraSet {
		opts, err := SplitArgs(cleanEnv(getenv("POSTGRES_EXTRA_OPTS")))
		if err != nil {
			return nil, fmt.Errorf("invalid POSTGRES_EXTRA_OPTS: %w", err)
		}
		cfg.PostgresExtraOpts = opts
	} else if !cfg.PostgresCluster && strings.HasSuffix(strings.ToLower(cfg.BackupSuffix), ".gz") {
		cfg.PostgresExtraOpts = []string{"-Z6"}
	}

	if cfg.KeepMins, err = parseIntDefault(cleanEnv(getenv("BACKUP_KEEP_MINS")), 1440, "BACKUP_KEEP_MINS"); err != nil {
		return nil, err
	}
	if cfg.KeepDays, err = parseIntDefault(cleanEnv(getenv("BACKUP_KEEP_DAYS")), 7, "BACKUP_KEEP_DAYS"); err != nil {
		return nil, err
	}
	if cfg.KeepWeeks, err = parseIntDefault(cleanEnv(getenv("BACKUP_KEEP_WEEKS")), 4, "BACKUP_KEEP_WEEKS"); err != nil {
		return nil, err
	}
	if cfg.KeepMonths, err = parseIntDefault(cleanEnv(getenv("BACKUP_KEEP_MONTHS")), 6, "BACKUP_KEEP_MONTHS"); err != nil {
		return nil, err
	}
	if cfg.HealthcheckPort, err = parseIntDefault(cleanEnv(getenv("HEALTHCHECK_PORT")), 8080, "HEALTHCHECK_PORT"); err != nil {
		return nil, err
	}
	if cfg.BackupMaxStaleHours, err = parseIntDefault(cleanEnv(getenv("BACKUP_MAX_STALE_HOURS")), 36, "BACKUP_MAX_STALE_HOURS"); err != nil {
		return nil, err
	}
	if cfg.DiskMaxUsagePct, err = parseIntDefault(cleanEnv(getenv("DISK_MAX_USAGE_PCT")), 92, "DISK_MAX_USAGE_PCT"); err != nil {
		return nil, err
	}
	if cfg.S3RetentionDays, err = parseIntDefault(cleanEnv(getenv("S3_RETENTION_DAYS")), 30, "S3_RETENTION_DAYS"); err != nil {
		return nil, err
	}
	if cfg.RestoreTestPort, err = parseIntDefault(cleanEnv(getenv("RESTORE_TEST_PORT")), 55432, "RESTORE_TEST_PORT"); err != nil {
		return nil, err
	}

	ageRecRaw, err := resolve("BACKUP_AGE_RECIPIENTS", "BACKUP_AGE_RECIPIENTS_FILE")
	if err != nil {
		return nil, err
	}
	cfg.AgeRecipients = splitList(ageRecRaw, true)

	if cfg.AgeIdentity, err = resolve("BACKUP_AGE_IDENTITY", "BACKUP_AGE_IDENTITY_FILE"); err != nil {
		return nil, err
	}
	if cfg.BackupPass, err = resolve("BACKUP_PASSWORD", "BACKUP_PASSWORD_FILE"); err != nil {
		return nil, err
	}

	s3Region := cleanEnv(getenv("S3_REGION"))
	if s3Region == "" {
		s3Region = cleanEnv(getenv("AWS_DEFAULT_REGION"))
	}
	cfg.S3Region = defaultStr(s3Region, "us-east-1")

	if cfg.S3AccessKeyID, err = resolve("S3_ACCESS_KEY_ID", "S3_ACCESS_KEY_ID_FILE"); err != nil {
		return nil, err
	}
	if cfg.S3AccessKeyID == "" {
		if cfg.S3AccessKeyID, err = resolve("AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID_FILE"); err != nil {
			return nil, err
		}
	}
	if cfg.S3SecretAccessKey, err = resolve("S3_SECRET_ACCESS_KEY", "S3_SECRET_ACCESS_KEY_FILE"); err != nil {
		return nil, err
	}
	if cfg.S3SecretAccessKey == "" {
		if cfg.S3SecretAccessKey, err = resolve("AWS_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY_FILE"); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// ValidateForBackup enforces invariants required before executing pg_dump / S3 / rotation.
func (c *Config) ValidateForBackup() error {
	if c.PostgresHost == "" {
		return fmt.Errorf("POSTGRES_HOST (or POSTGRES_PORT_5432_TCP_ADDR) is required")
	}
	if c.PostgresPort < 1 || c.PostgresPort > 65535 {
		return fmt.Errorf("POSTGRES_PORT out of range: %d", c.PostgresPort)
	}
	if c.PostgresUser == "" {
		return fmt.Errorf("POSTGRES_USER (or POSTGRES_USER_FILE) is required")
	}
	if c.PostgresPassword == "" && c.PostgresPassfileStore == "" {
		return fmt.Errorf("POSTGRES_PASSWORD, POSTGRES_PASSWORD_FILE, or POSTGRES_PASSFILE_STORE is required")
	}
	if len(c.PostgresDBs) == 0 {
		return fmt.Errorf("POSTGRES_DB (or POSTGRES_DB_FILE) is required")
	}
	switch c.BackupLatestType {
	case "symlink", "hardlink", "none":
	default:
		return fmt.Errorf("BACKUP_LATEST_TYPE must be symlink, hardlink, or none (got %q)", c.BackupLatestType)
	}
	if c.KeepMins < 0 || c.KeepDays < 0 || c.KeepWeeks < 0 || c.KeepMonths < 0 {
		return fmt.Errorf("retention values (BACKUP_KEEP_*) must be non-negative")
	}
	if c.DiskMaxUsagePct < 1 || c.DiskMaxUsagePct > 100 {
		return fmt.Errorf("DISK_MAX_USAGE_PCT must be between 1 and 100 (got %d)", c.DiskMaxUsagePct)
	}
	if len(c.AgeRecipients) > 0 && c.BackupPass != "" {
		return fmt.Errorf("set either BACKUP_AGE_RECIPIENTS or BACKUP_PASSWORD, not both")
	}
	if c.S3Bucket != "" {
		if c.S3RequireEncryption && !c.EncryptionEnabled() {
			return fmt.Errorf("S3_BUCKET is set and S3_REQUIRE_ENCRYPTION=true, but neither BACKUP_AGE_RECIPIENTS nor BACKUP_PASSWORD is configured (fail-closed)")
		}
		if c.S3AccessKeyID == "" || c.S3SecretAccessKey == "" {
			return fmt.Errorf("S3_ACCESS_KEY_ID/AWS_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY/AWS_SECRET_ACCESS_KEY are required when S3_BUCKET is set")
		}
		if c.S3RetentionDays <= 0 {
			return fmt.Errorf("S3_RETENTION_DAYS must be > 0 (got %d)", c.S3RetentionDays)
		}
	}
	return nil
}

// ValidateDirWritable checks that BACKUP_DIR exists and can accept files.
func (c *Config) ValidateDirWritable() error {
	info, err := os.Stat(c.BackupDir)
	if err != nil {
		return fmt.Errorf("stat BACKUP_DIR %q: %w", c.BackupDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("BACKUP_DIR %q is not a directory", c.BackupDir)
	}
	probe, err := os.CreateTemp(c.BackupDir, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("BACKUP_DIR %q is not writable: %w", c.BackupDir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// SplitArgs splits a command-line option string respecting single/double quotes and backslashes.
func SplitArgs(input string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inToken := false
	var quote rune

	runes := []rune(input)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			if r == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				continue
			}
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
			continue
		}
		switch {
		case unicode.IsSpace(r):
			if inToken {
				args = append(args, cur.String())
				cur.Reset()
				inToken = false
			}
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inToken = true
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in %q", input)
	}
	if inToken {
		args = append(args, cur.String())
	}
	return args, nil
}

func cleanEnv(v string) string {
	v = strings.TrimSpace(v)
	if v == NoneSentinel {
		return ""
	}
	return v
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func parseBool(v string, fallback bool) bool {
	if v == "" {
		return fallback
	}
	switch strings.ToUpper(v) {
	case "TRUE", "1", "YES", "ON":
		return true
	case "FALSE", "0", "NO", "OFF":
		return false
	default:
		return fallback
	}
}

func parseIntDefault(v string, fallback int, name string) (int, error) {
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer for %s (%q): %w", name, v, err)
	}
	return n, nil
}

func splitList(raw string, stripComments bool) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if stripComments {
			if idx := strings.IndexByte(line, '#'); idx >= 0 {
				line = line[:idx]
			}
		}
		parts := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		})
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
