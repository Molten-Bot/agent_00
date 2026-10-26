//go:build linux

package workspace

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
	if err == nil {
		t.Fatal("checkWorkspaceSpace(low space) error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "has only") {
		t.Fatalf("checkWorkspaceSpace(low space) error = %q, want mention of free space shortfall", err.Error())
	}
}
