package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadProdrigestivillDefaultsAndFiles(t *testing.T) {
	tmp := t.TempDir()
	dbFile := filepath.Join(tmp, "dbs")
	passFile := filepath.Join(tmp, "pass")
	recFile := filepath.Join(tmp, "recipients.txt")

	if err := os.WriteFile(dbFile, []byte("db_one, db_two\ndb_three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passFile, []byte("super-secret\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recFile, []byte("# comment\nage1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"POSTGRES_PORT_5432_TCP_ADDR": "pg-legacy",
		"POSTGRES_PORT_5432_TCP_PORT": "5439",
		"POSTGRES_USER":               "backup_user",
		"POSTGRES_PASSWORD_FILE":      passFile,
		"POSTGRES_DB":                 NoneSentinel,
		"POSTGRES_DB_FILE":            dbFile,
		"BACKUP_AGE_RECIPIENTS_FILE":  recFile,
		"WEBHOOK_URL":                 NoneSentinel,
		"POSTGRES_EXTRA_OPTS":         `-Z1 --schema="public data"`,
	}

	cfg, err := Load(mapEnv(env), os.ReadFile)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}

	if cfg.PostgresHost != "pg-legacy" || cfg.PostgresPort != 5439 {
		t.Fatalf("unexpected legacy host/port: %s:%d", cfg.PostgresHost, cfg.PostgresPort)
	}
	if cfg.PostgresPassword != "super-secret" {
		t.Fatalf("expected trimmed password, got %q", cfg.PostgresPassword)
	}
	wantDBs := []string{"db_one", "db_two", "db_three"}
	if !reflect.DeepEqual(cfg.PostgresDBs, wantDBs) {
		t.Fatalf("got DBs %v, want %v", cfg.PostgresDBs, wantDBs)
	}
	wantOpts := []string{"-Z1", "--schema=public data"}
	if !reflect.DeepEqual(cfg.PostgresExtraOpts, wantOpts) {
		t.Fatalf("got ExtraOpts %v, want %v", cfg.PostgresExtraOpts, wantOpts)
	}
	if cfg.WebhookURL != "" {
		t.Fatalf("expected **None** webhook to clear, got %q", cfg.WebhookURL)
	}
	if len(cfg.AgeRecipients) != 1 {
		t.Fatalf("expected 1 recipient from file, got %v", cfg.AgeRecipients)
	}
	if err := cfg.ValidateForBackup(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidateFailClosedOnUnencryptedS3(t *testing.T) {
	env := map[string]string{
		"POSTGRES_HOST":         "postgres",
		"POSTGRES_USER":         "app",
		"POSTGRES_PASSWORD":     "secret",
		"POSTGRES_DB":           "appdb",
		"S3_BUCKET":             "backups-prod",
		"AWS_ACCESS_KEY_ID":     "ak",
		"AWS_SECRET_ACCESS_KEY": "sk",
	}
	cfg, err := Load(mapEnv(env), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.ValidateForBackup()
	if err == nil || !strings.Contains(err.Error(), "fail-closed") {
		t.Fatalf("expected fail-closed S3 encryption error, got: %v", err)
	}

	// Setting BACKUP_PASSWORD satisfies fail-closed rule
	env["BACKUP_PASSWORD"] = "enc-pass"
	cfg, err = Load(mapEnv(env), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateForBackup(); err != nil {
		t.Fatalf("expected valid config with BACKUP_PASSWORD, got: %v", err)
	}

	// Setting both BACKUP_PASSWORD and BACKUP_AGE_RECIPIENTS must fail
	env["BACKUP_AGE_RECIPIENTS"] = "age1testkey"
	cfg, err = Load(mapEnv(env), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateForBackup(); err == nil {
		t.Fatal("expected error when both BACKUP_AGE_RECIPIENTS and BACKUP_PASSWORD are set")
	}
}
