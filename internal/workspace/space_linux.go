//go:build linux

package workspace

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// CheckCacheMounts rejects mounts between root and path or within path,
// including bind mounts on the same filesystem.
func CheckCacheMounts(root, path string) error {
	f, err := os.Open(procSelfMountInfoPath)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		point, _, ok := parseMountInfoLine(scanner.Text())
		if ok && cacheMountIntersects(root, path, point) {
			return fmt.Errorf("cache tree contains mount %s", point)
		}
	}
	return scanner.Err()
}

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
	return checkWorkspaceInodes(path, stat)
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
	return checkWorkspaceInodes(path, stat)
}

func checkWorkspaceInodes(path string, stat syscall.Statfs_t) error {
	// Filesystems without inode accounting can report zero for both counts.
	if stat.Files > 0 && stat.Ffree == 0 {
		return fmt.Errorf("workspace storage %s has no free inodes; free up workspace storage or move it to another filesystem: %w", path, syscall.ENOSPC)
	}
	return nil
}
