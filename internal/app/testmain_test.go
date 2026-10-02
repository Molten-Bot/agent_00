package app

import (
	"os"
	"path/filepath"
	"testing"
)

var testWorkspaceBase string

func TestMain(m *testing.M) {
	authRoot, err := os.MkdirTemp("", "agent00-test-auth-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CODEX_HOME", filepath.Join(authRoot, "codex"))
	_ = os.Unsetenv("GH_TOKEN")
	_ = os.Unsetenv("GITHUB_TOKEN")
	testWorkspaceBase = filepath.Join(authRoot, "workspaces")
	_ = os.Setenv("HARNESS_WORKSPACE_RAM_BASE", testWorkspaceBase)
	_ = os.Setenv("HARNESS_WORKSPACE_DISK_BASE", testWorkspaceBase)
	_ = os.Unsetenv("HARNESS_WORKSPACE_ROOT_NAME")
	code := m.Run()
	_ = os.RemoveAll(authRoot)
	os.Exit(code)
}
