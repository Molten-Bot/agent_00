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
	// Claude's persistent configuration directory is resolved before task HOME
	// isolation, so without this every Claude run test would repair, probe and
	// create entries in the operator's own directory.
	_ = os.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(authRoot, "claude"))
	_ = os.Unsetenv("GH_TOKEN")
	_ = os.Unsetenv("GITHUB_TOKEN")
	_ = os.Unsetenv("HARNESS_AGENT_HARNESS")
	_ = os.Unsetenv("HARNESS_AGENT_COMMAND")
	testWorkspaceBase = filepath.Join(authRoot, "workspaces")
	_ = os.Setenv("HARNESS_WORKSPACE_RAM_BASE", testWorkspaceBase)
	_ = os.Setenv("HARNESS_WORKSPACE_DISK_BASE", testWorkspaceBase)
	_ = os.Unsetenv("HARNESS_WORKSPACE_ROOT_NAME")
	code := m.Run()
	_ = os.RemoveAll(authRoot)
	os.Exit(code)
}
