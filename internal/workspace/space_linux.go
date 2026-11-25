//go:build linux

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// statfsFunc is overridden in tests to simulate low-disk-space conditions
// without requiring a real filesystem with little free space.
var statfsFunc = syscall.Statfs

// CheckPersistentStorage detects memory-backed mounts, including mounts that
// shadow a persistent parent or are reached through a symlink.
func CheckPersistentStorage(path string) error {
	probe := filepath.Clean(path)
	var stat syscall.Statfs_t
	for {
		err := statfsFunc(probe, &stat)
		if err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if !os.IsNotExist(err) || parent == probe {
			return fmt.Errorf("inspect workspace storage %s: %w", path, err)
		}
		probe = parent
	}
	if kind := uint32(stat.Type); kind == unix.TMPFS_MAGIC || kind == unix.RAMFS_MAGIC {
		return fmt.Errorf("%w: %s is on a memory-backed filesystem (free_bytes=%d total_bytes=%d); mount persistent disk storage there",
			ErrUnsuitableStorage, path, stat.Bavail*uint64(stat.Bsize), stat.Blocks*uint64(stat.Bsize))
	}
	return nil
}

// Reserve room for a checkout and its initial tooling. This check is deliberately
// uncached: a long-running daemon can fill a previously executable tmpfs.
func checkWorkspaceSpace(path string) error {
	var stat syscall.Statfs_t
	if err := statfsFunc(path, &stat); err != nil {
		return fmt.Errorf("inspect workspace storage %s: %w", path, err)
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if available < MinimumFreeBytes {
		return fmt.Errorf("workspace storage %s has only %d MiB free; at least 512 MiB is required; configure HARNESS_WORKSPACE_DISK_BASE on a disk-backed filesystem: %w", path, available/(1024*1024), syscall.ENOSPC)
	}
	return nil
}
