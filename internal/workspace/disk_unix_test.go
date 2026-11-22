//go:build linux || darwin

package workspace

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDiskUsageCountsAllocatedDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths := []string{root, filepath.Join(root, "nested"), filepath.Join(root, "nested", "empty")}
	if err := os.MkdirAll(paths[2], 0o700); err != nil {
		t.Fatal(err)
	}
	var want uint64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want += uint64(info.Sys().(*syscall.Stat_t).Blocks) * 512
	}
	got, err := DiskUsage(root)
	if err != nil || got != want {
		t.Fatalf("DiskUsage() = %d, %v, want %d allocated directory bytes", got, err, want)
	}
}
