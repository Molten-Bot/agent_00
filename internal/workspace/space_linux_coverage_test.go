//go:build linux

package workspace

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestCheckWorkspaceSpaceReturnsErrorForNonexistentPath exercises the real
// statfs error path: no fake/seam is needed since a nonexistent path
// reliably fails the syscall on every Linux environment.
func TestCheckWorkspaceSpaceReturnsErrorForNonexistentPath(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	err := checkWorkspaceSpace(missing)
	if err == nil {
		t.Fatal("checkWorkspaceSpace(missing path) error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "inspect workspace storage") {
		t.Fatalf("checkWorkspaceSpace(missing path) error = %q, want mention of inspect failure", err.Error())
	}
}

func TestCheckPersistentStorageDetectsMemoryMountOverDisk(t *testing.T) {
	orig := statfsFunc
	t.Cleanup(func() { statfsFunc = orig })
	statfsFunc = func(path string, stat *syscall.Statfs_t) error {
		switch path {
		case "/config":
			stat.Type = unix.EXT4_SUPER_MAGIC
		case "/config/issue-workspaces":
			stat.Type = unix.TMPFS_MAGIC
			stat.Bsize = 4096
			stat.Blocks = 2_000_000
			stat.Bavail = 1_000_000
		default:
			return syscall.ENOENT
		}
		return nil
	}
	if err := CheckPersistentStorage("/config"); err != nil {
		t.Fatalf("disk-backed parent rejected: %v", err)
	}
	err := CheckPersistentStorage("/config/issue-workspaces/new-claim")
	if !errors.Is(err, ErrUnsuitableStorage) || !strings.Contains(err.Error(), "free_bytes=4096000000 total_bytes=8192000000") {
		t.Fatalf("nested memory mount error = %v", err)
	}
}

// TestCheckWorkspaceSpaceRejectsLowAvailableSpace fakes the statfs syscall
// via the injected statfsFunc seam because reliably provisioning an
// almost-full filesystem in a sandboxed test environment is impractical.
func TestCheckWorkspaceSpaceRejectsLowAvailableSpace(t *testing.T) {
	orig := statfsFunc
	t.Cleanup(func() { statfsFunc = orig })

	statfsFunc = func(path string, stat *syscall.Statfs_t) error {
		*stat = syscall.Statfs_t{
			Bsize:  4096,
			Bavail: 10, // 10 * 4096 bytes, far below the 512 MiB reserve.
		}
		return nil
	}

	err := checkWorkspaceSpace("/irrelevant/path")
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("checkWorkspaceSpace(low space) error = %v, want ENOSPC", err)
	}
	if !strings.Contains(err.Error(), "has only") {
		t.Fatalf("checkWorkspaceSpace(low space) error = %q, want mention of free space shortfall", err.Error())
	}
}
