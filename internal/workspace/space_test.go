package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceFallsBackWhenRAMFillsAfterStartup(t *testing.T) {
	full := false
	m := Manager{
		PathExists: func(string) bool { return true }, CanExec: func(string) bool { return true },
		MkdirAll: func(string, os.FileMode) error { return nil }, NewGUID: func() string { return "run" },
		CheckSpace: func(path string) error {
			if full && strings.HasPrefix(path, defaultRAMBase) {
				return errors.New("workspace storage full")
			}
			return nil
		},
	}
	if err := m.PrepareRoots(); err != nil {
		t.Fatal(err)
	}
	full = true
	path, _, err := m.CreateRunDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(defaultDiskBase, defaultWorkspaceRoot, "run"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestWorkspaceRejectsFullFallbackBeforeCloning(t *testing.T) {
	m := Manager{
		PathExists: func(string) bool { return true }, CanExec: func(string) bool { return true },
		MkdirAll: func(string, os.FileMode) error { return nil }, NewGUID: func() string { return "run" },
		CheckSpace: func(string) error { return errors.New("workspace storage full") },
	}
	if err := m.PrepareRoots(); err == nil {
		t.Fatal("PrepareRoots accepted full storage")
	}
	if _, _, err := m.CreateRunDir(); err == nil {
		t.Fatal("CreateRunDir accepted full storage")
	}
}
