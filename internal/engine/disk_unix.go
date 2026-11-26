//go:build !windows

package engine

import (
	"fmt"
	"syscall"
)

// CheckDiskUsage verifies that disk usage on dir is strictly below maxUsagePct.
func CheckDiskUsage(dir string, maxUsagePct int) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %q: %w", dir, err)
	}
	if st.Blocks == 0 {
		return nil
	}
	used := st.Blocks - st.Bavail
	pct := int((used * 100) / st.Blocks)
	if pct >= maxUsagePct {
		return fmt.Errorf("disk usage at %d%% (>= %d%% limit) on %q", pct, maxUsagePct, dir)
	}
	return nil
}
