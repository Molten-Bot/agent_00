package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	authRoot, err := os.MkdirTemp("", "agent00-test-auth-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CODEX_HOME", filepath.Join(authRoot, "codex"))
	_ = os.Unsetenv("GH_TOKEN")
	_ = os.Unsetenv("GITHUB_TOKEN")
	_ = os.Unsetenv("HARNESS_WORKSPACE_RAM_BASE")
	_ = os.Unsetenv("HARNESS_WORKSPACE_DISK_BASE")
	code := m.Run()
	_ = os.RemoveAll(authRoot)
	os.Exit(code)
}
