package engine

import (
	"compress/gzip"
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/phantomwar/ad-postgres-backup/internal/config"
	"github.com/phantomwar/ad-postgres-backup/internal/crypto"
	"github.com/phantomwar/ad-postgres-backup/internal/s3sync"
)

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, name string, args []string, _ []string, _ io.Reader, stdout io.Writer) error {
	if name == "pg_dump" {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-f" {
				f, err := os.OpenFile(args[i+1], os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				gz := gzip.NewWriter(f)
				_, _ = gz.Write([]byte("CREATE TABLE t (id int);\n"))
				_ = gz.Close()
				return f.Close()
			}
		}
	}
	if name == "pg_dumpall" && stdout != nil {
		_, err := io.WriteString(stdout, "CREATE ROLE postgres;\n")
		return err
	}
	return nil
}

func TestRunCycleEncryptsArchivesAndUploadsToS3(t *testing.T) {
	pub, sec, err := crypto.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	uploadedKeys := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			uploadedKeys[r.URL.Path] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			type listResp struct {
				XMLName xml.Name `xml:"ListBucketResult"`
				Name    string   `xml:"Name"`
			}
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(listResp{Name: "bucket1"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	backupDir := t.TempDir()
	storageDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(storageDir, "doc.txt"), []byte("uploaded asset"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		PostgresHost:        "localhost",
		PostgresPort:        5432,
		PostgresUser:        "user",
		PostgresPassword:    "pass",
		PostgresDBs:         []string{"appdb"},
		PostgresExtraOpts:   []string{"-Z1"},
		BackupDir:           backupDir,
		BackupSuffix:        ".sql.gz",
		BackupLatestType:    "hardlink",
		KeepMins:            60,
		KeepDays:            7,
		KeepWeeks:           4,
		KeepMonths:          6,
		BackupChecksum:      true,
		DiskMaxUsagePct:     95,
		AgeRecipients:       []string{pub},
		StorageDir:          storageDir,
		StoragePrefix:       "storage",
		S3Bucket:            "bucket1",
		S3Endpoint:          srv.URL,
		S3Region:            "us-east-1",
		S3AccessKeyID:       "ak",
		S3SecretAccessKey:   "sk",
		S3ForcePathStyle:    true,
		S3RetentionDays:     30,
		S3RequireEncryption: true,
	}

	s3Client, err := s3sync.NewClient(context.Background(), s3sync.Options{
		Endpoint:          cfg.S3Endpoint,
		Bucket:            cfg.S3Bucket,
		Region:            cfg.S3Region,
		AccessKeyID:       cfg.S3AccessKeyID,
		SecretAccessKey:   cfg.S3SecretAccessKey,
		ForcePathStyle:    true,
		RequireEncryption: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	artifacts, err := RunCycle(context.Background(), logger, cfg, fakeRunner{}, s3Client)
	if err != nil {
		t.Fatalf("RunCycle failed: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("expected 2 artifacts (appdb + storage), got %d", len(artifacts))
	}
	for _, a := range artifacts {
		if !a.Encrypted || !a.S3Synced {
			t.Fatalf("expected encrypted and s3 synced artifact, got %+v", a)
		}
		if err := crypto.VerifySHA256File(a.Path); err != nil {
			t.Fatalf("VerifySHA256File(%s): %v", a.Path, err)
		}
		decOut := filepath.Join(t.TempDir(), "plain")
		if err := crypto.DecryptFile(a.Path, decOut, crypto.DecryptionConfig{Identity: sec}); err != nil {
			t.Fatalf("DecryptFile(%s): %v", a.Path, err)
		}
	}

	if len(uploadedKeys) != 4 {
		t.Fatalf("expected 4 S3 objects (2 .age + 2 .sha256), got %d", len(uploadedKeys))
	}
	for k := range uploadedKeys {
		if !strings.HasSuffix(k, ".age") && !strings.HasSuffix(k, ".age.sha256") {
			t.Fatalf("unexpected S3 key %q", k)
		}
	}
}
