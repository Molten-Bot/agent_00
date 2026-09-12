//go:build linux

package workspace

import (
	"fmt"
	"syscall"
)

// Reserve room for a checkout and its initial tooling. This check is deliberately
// uncached: a long-running daemon can fill a previously executable tmpfs.
func checkWorkspaceSpace(path string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("inspect workspace storage %s: %w", path, err)
	}
	const reserve = uint64(512 * 1024 * 1024)
	available := stat.Bavail * uint64(stat.Bsize)
	if available < reserve {
		return fmt.Errorf("workspace storage %s has only %d MiB free; at least 512 MiB is required; configure HARNESS_WORKSPACE_DISK_BASE on a disk-backed filesystem", path, available/(1024*1024))
	}
	return nil
}
