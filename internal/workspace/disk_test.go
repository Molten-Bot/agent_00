//go:build linux || darwin || windows

package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDiskSpaceUsesExistingParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	device, _, err := DiskSpace(root)
	if err != nil {
		t.Fatal(err)
	}
	got, free, err := DiskSpace(filepath.Join(root, "missing", "workspace"))
	if err != nil || got != device || free == 0 {
		t.Fatalf("DiskSpace(missing workspace) = %q, %d, %v", got, free, err)
	}
}

func TestDiskUsageIncludesGitDataAndSkipsSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index"), []byte("fixture git data"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := DiskUsage(root)
	if err != nil || before == 0 {
		t.Fatalf("DiskUsage() = %d, %v, want nonzero Git usage", before, err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, make([]byte, 64*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if after, err := DiskUsage(root); err != nil || after != before {
		t.Fatalf("DiskUsage() with symlink = %d, %v, want %d", after, err, before)
	}
	if runtime.GOOS != "windows" {
		if err := os.Link(filepath.Join(gitDir, "index"), filepath.Join(root, "hardlink")); err != nil {
			t.Fatal(err)
		}
		if after, err := DiskUsage(root); err != nil || after != before {
			t.Fatalf("DiskUsage() with hard link = %d, %v, want %d", after, err, before)
		}
	}
}
