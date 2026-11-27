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
		return fmt.Errorf("workspace storage %s has only %d MiB free on its filesystem (capacity %d MiB); at least 512 MiB is required; free space or expand the filesystem backing this path: %w",
			path, available/(1024*1024), stat.Blocks*uint64(stat.Bsize)/(1024*1024), syscall.ENOSPC)
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

// CheckNoMountsWithin detects separately mounted data inside a task directory,
// including bind mounts on the same filesystem.
func CheckNoMountsWithin(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	f, err := os.Open(procSelfMountInfoPath)
	if err != nil {
		return fmt.Errorf("inspect workspace mounts: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		mount, _, ok := parseMountInfoLine(scanner.Text())
		if ok && pathWithinMount(mount, resolved) {
			return fmt.Errorf("workspace contains a mount at %s; unmount it before cleanup", mount)
		}
	}
	return scanner.Err()
}
