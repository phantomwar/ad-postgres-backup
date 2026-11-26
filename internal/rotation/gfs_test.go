package rotation

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRotateGFSAndPrunePreservesHardlinks(t *testing.T) {
	backupDir := t.TempDir()
	now := time.Date(2026, 11, 25, 14, 30, 5, 0, time.UTC)

	lastDir := filepath.Join(backupDir, "last")
	if err := os.MkdirAll(lastDir, 0o750); err != nil {
		t.Fatal(err)
	}
	tmpDump := filepath.Join(lastDir, ".tmp-appdb")
	tmpSum := tmpDump + ".sha256"
	if err := os.WriteFile(tmpDump, []byte("dump-payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpSum, []byte("deadbeef  appdb\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	latestType := "symlink"
	if runtime.GOOS == "windows" {
		latestType = "hardlink"
	}

	finalLast, err := RotateGFS(backupDir, "appdb", ".sql.gz.age", tmpDump, tmpSum, latestType, now)
	if err != nil {
		t.Fatalf("RotateGFS failed: %v", err)
	}

	dailyPath := filepath.Join(backupDir, "daily", "appdb-20261125.sql.gz.age")
	weeklyPath := filepath.Join(backupDir, "weekly", "appdb-202648.sql.gz.age")
	monthlyPath := filepath.Join(backupDir, "monthly", "appdb-202611.sql.gz.age")

	for _, p := range []string{finalLast, dailyPath, weeklyPath, monthlyPath, dailyPath + ".sha256"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to exist: %v", p, err)
		}
	}

	// Age only the last/ entry beyond KeepMins (e.g. 2 days ago) while keeping daily/ fresh
	oldTime := now.Add(-48 * time.Hour)
	if err := os.Chtimes(finalLast, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(finalLast+".sha256", oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	removed, err := Prune(backupDir, "appdb", ".sql.gz.age", now, KeepPolicy{
		Mins:   60,
		Days:   7,
		Weeks:  4,
		Months: 6,
	})
	if err != nil {
		t.Fatalf("Prune failed: %v", err)
	}
	if removed != 2 {
		t.Fatalf("expected 2 removed files from last/ (dump + sha256), got %d", removed)
	}

	// Hardlink in daily/ must survive last/ removal intact
	data, err := os.ReadFile(dailyPath)
	if err != nil {
		t.Fatalf("expected daily hardlink to survive: %v", err)
	}
	if string(data) != "dump-payload" {
		t.Fatalf("unexpected daily payload: %q", data)
	}

	// Latest DB backup finder skips storage archives and locates dailyPath
	found, err := FindLatestDatabaseBackup(backupDir, "storage")
	if err != nil {
		t.Fatalf("FindLatestDatabaseBackup failed: %v", err)
	}
	if found != dailyPath {
		t.Fatalf("got latest backup %q, want %q", found, dailyPath)
	}
}
