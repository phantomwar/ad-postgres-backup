//go:build windows

package engine

// CheckDiskUsage is a no-op stub on Windows builds; Linux containers enforce statfs limits.
func CheckDiskUsage(_ string, _ int) error {
	return nil
}
