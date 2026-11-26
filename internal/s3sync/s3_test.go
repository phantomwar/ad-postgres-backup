package s3sync

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUploadAndPruneAgainstMockS3(t *testing.T) {
	var mu sync.Mutex
	uploaded := map[string]string{}
	deletedKeys := []string{}

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-45 * 24 * time.Hour).Format(time.RFC3339)
	freshTime := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			uploaded[r.URL.Path] = string(body)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			type item struct {
				Key          string `xml:"Key"`
				LastModified string `xml:"LastModified"`
				Size         int    `xml:"Size"`
			}
			type resp struct {
				XMLName     xml.Name `xml:"ListBucketResult"`
				Name        string   `xml:"Name"`
				IsTruncated bool     `xml:"IsTruncated"`
				Contents    []item   `xml:"Contents"`
			}
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(resp{
				Name: "backups",
				Contents: []item{
					{Key: "prod/appdb-old.sql.gz.age", LastModified: oldTime, Size: 10},
					{Key: "prod/appdb-new.sql.gz.age", LastModified: freshTime, Size: 12},
				},
			})
		case http.MethodDelete:
			deletedKeys = append(deletedKeys, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	client, err := NewClient(context.Background(), Options{
		Endpoint:          srv.URL,
		Bucket:            "backups",
		Region:            "us-east-1",
		Prefix:            "prod",
		AccessKeyID:       "ak",
		SecretAccessKey:   "sk",
		ForcePathStyle:    true,
		RequireEncryption: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	plain := filepath.Join(tmp, "appdb-20261125.sql.gz")
	if err := os.WriteFile(plain, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.UploadBackup(context.Background(), plain); err == nil {
		t.Fatal("expected fail-closed error uploading unencrypted file")
	}

	enc := filepath.Join(tmp, "appdb-20261125.sql.gz.age")
	if err := os.WriteFile(enc, []byte("encrypted-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enc+".sha256", []byte("abc  appdb\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := client.UploadBackup(context.Background(), enc); err != nil {
		t.Fatalf("UploadBackup failed: %v", err)
	}
	if uploaded["/backups/prod/appdb-20261125.sql.gz.age"] != "encrypted-bytes" {
		t.Fatalf("unexpected uploaded objects: %+v", uploaded)
	}
	if _, ok := uploaded["/backups/prod/appdb-20261125.sql.gz.age.sha256"]; !ok {
		t.Fatalf("expected sha256 sidecar upload, got: %+v", uploaded)
	}

	n, err := client.PruneExpired(context.Background(), 30, now)
	if err != nil {
		t.Fatalf("PruneExpired failed: %v", err)
	}
	if n != 1 || len(deletedKeys) != 1 || !strings.HasSuffix(deletedKeys[0], "appdb-old.sql.gz.age") {
		t.Fatalf("expected only old object deleted, got n=%d keys=%v", n, deletedKeys)
	}
}
