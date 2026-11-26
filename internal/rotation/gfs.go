package rotation

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// KeepPolicy defines the GFS retention windows matching prodrigestivill/postgres-backup-local.
type KeepPolicy struct {
	Mins   int
	Days   int
	Weeks  int
	Months int
}

// Tiers returns the canonical GFS subdirectory names.
var Tiers = []string{"last", "daily", "weekly", "monthly"}

// RotateGFS moves tmpDataPath into BACKUP_DIR/last/<name>-YYYYMMDD-HHMMSS<ext>
// (along with optional tmpSumPath -> <dest>.sha256), hardlinks it into daily/, weekly/, and monthly/,
// and updates <name>-latest<ext> pointers according to latestType ("symlink", "hardlink", or "none").
func RotateGFS(backupDir, name, ext, tmpDataPath, tmpSumPath, latestType string, now time.Time) (string, error) {
	for _, tier := range Tiers {
		if err := os.MkdirAll(filepath.Join(backupDir, tier), 0o755); err != nil {
			return "", fmt.Errorf("create tier dir %s: %w", tier, err)
		}
	}

	isoYear, isoWeek := now.ISOWeek()
	lastName := fmt.Sprintf("%s-%s%s", name, now.Format("20060102-150405"), ext)
	dailyName := fmt.Sprintf("%s-%s%s", name, now.Format("20060102"), ext)
	weeklyName := fmt.Sprintf("%s-%04d%02d%s", name, isoYear, isoWeek, ext)
	monthlyName := fmt.Sprintf("%s-%s%s", name, now.Format("200601"), ext)

	lastPath := filepath.Join(backupDir, "last", lastName)
	if err := os.Rename(tmpDataPath, lastPath); err != nil {
		return "", fmt.Errorf("promote backup to last/: %w", err)
	}

	hasSum := tmpSumPath != ""
	lastSumPath := lastPath + ".sha256"
	if hasSum {
		if err := os.Rename(tmpSumPath, lastSumPath); err != nil {
			return "", fmt.Errorf("promote sha256 to last/: %w", err)
		}
	}

	tierTargets := []struct {
		tier     string
		filename string
	}{
		{"daily", dailyName},
		{"weekly", weeklyName},
		{"monthly", monthlyName},
	}

	for _, t := range tierTargets {
		dstPath := filepath.Join(backupDir, t.tier, t.filename)
		if err := replaceHardlink(lastPath, dstPath); err != nil {
			return "", fmt.Errorf("hardlink %s/%s: %w", t.tier, t.filename, err)
		}
		if hasSum {
			if err := replaceHardlink(lastSumPath, dstPath+".sha256"); err != nil {
				return "", fmt.Errorf("hardlink %s/%s.sha256: %w", t.tier, t.filename, err)
			}
		}
	}

	latestName := fmt.Sprintf("%s-latest%s", name, ext)
	allTiers := append([]struct {
		tier     string
		filename string
	}{{"last", lastName}}, tierTargets...)

	for _, t := range allTiers {
		tierDir := filepath.Join(backupDir, t.tier)
		targetPath := filepath.Join(tierDir, t.filename)
		latestPath := filepath.Join(tierDir, latestName)
		if err := updateLatestPointer(latestType, t.filename, targetPath, latestPath); err != nil {
			return "", fmt.Errorf("update latest pointer in %s: %w", t.tier, err)
		}
		if hasSum {
			if err := updateLatestPointer(latestType, t.filename+".sha256", targetPath+".sha256", latestPath+".sha256"); err != nil {
				return "", fmt.Errorf("update latest sha256 pointer in %s: %w", t.tier, err)
			}
		}
	}

	return lastPath, nil
}

// Prune removes expired backup artifacts and their .sha256 sidecars from GFS tiers.
// It never removes *-latest* pointers or files belonging to other database prefixes.
func Prune(backupDir, name, ext string, now time.Time, keep KeepPolicy) (int, error) {
	windows := []struct {
		tier   string
		maxAge time.Duration
	}{
		{"last", time.Duration(keep.Mins) * time.Minute},
		{"daily", time.Duration(keep.Days) * 24 * time.Hour},
		{"weekly", time.Duration(keep.Weeks*7+1) * 24 * time.Hour},
		{"monthly", time.Duration(keep.Months*31+1) * 24 * time.Hour},
	}

	prefix := name + "-"
	latestBase := fmt.Sprintf("%s-latest%s", name, ext)
	latestSum := latestBase + ".sha256"
	removed := 0

	for _, w := range windows {
		dir := filepath.Join(backupDir, w.tier)
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("read dir %s: %w", dir, err)
		}
		cutoff := now.Add(-w.maxAge)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			fname := entry.Name()
			if fname == latestBase || fname == latestSum {
				continue
			}
			if !strings.HasPrefix(fname, prefix) {
				continue
			}
			if !strings.HasSuffix(fname, ext) && !strings.HasSuffix(fname, ext+".sha256") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(dir, fname)); err == nil {
					removed++
				}
			}
		}
	}
	return removed, nil
}

// FindLatestDatabaseBackup returns the newest database dump in backupDir (searching last/ then daily/).
// It ignores sidecars (.sha256), temp files (.tmp-*), *-latest* pointers, and companion storage archives.
func FindLatestDatabaseBackup(backupDir, storagePrefix string) (string, error) {
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate

	for _, sub := range []string{"last", "daily", ""} {
		dir := backupDir
		if sub != "" {
			dir = filepath.Join(backupDir, sub)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".sha256") || strings.Contains(name, "-latest") {
				continue
			}
			if storagePrefix != "" && strings.HasPrefix(name, storagePrefix+"-") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			candidates = append(candidates, candidate{
				path:    filepath.Join(dir, name),
				modTime: info.ModTime(),
			})
		}
		if len(candidates) > 0 {
			break
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no database backup files found in %q", backupDir)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].path > candidates[j].path
		}
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	return candidates[0].path, nil
}

func replaceHardlink(src, dst string) error {
	_ = os.Remove(dst)
	return os.Link(src, dst)
}

func updateLatestPointer(latestType, relTarget, absTarget, linkPath string) error {
	switch latestType {
	case "none":
		return nil
	case "hardlink":
		return replaceHardlink(absTarget, linkPath)
	case "symlink":
		_ = os.Remove(linkPath)
		return os.Symlink(relTarget, linkPath)
	default:
		return fmt.Errorf("unsupported BACKUP_LATEST_TYPE %q", latestType)
	}
}
