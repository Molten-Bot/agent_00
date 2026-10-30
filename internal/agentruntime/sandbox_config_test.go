package agentruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeSandboxConfigRootCreatesMissingConfigDirectory(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "claude-config")

	resolved, err := claudeSandboxConfigRoot([]string{"CLAUDE_CONFIG_DIR=" + configDir}, workspace)
	if err != nil {
		t.Fatalf("claudeSandboxConfigRoot() error = %v", err)
	}
	if resolved == "" {
		t.Fatal("claudeSandboxConfigRoot() granted no writable root for a missing config directory")
	}
	info, statErr := os.Stat(configDir)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("CLAUDE_CONFIG_DIR was not created: stat error = %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("CLAUDE_CONFIG_DIR mode = %v, want 0700", got)
	}
}

func TestClaudeSandboxConfigRootReportsUnresolvableConfigDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "missing"), dangling); err != nil {
		t.Fatal(err)
	}

	_, err := claudeSandboxConfigRoot([]string{"CLAUDE_CONFIG_DIR=" + dangling}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("claudeSandboxConfigRoot() error = %v, want the resolve failure reported", err)
	}
}

func TestClaudeSandboxConfigRootRejectsSharedTopLevelDirectories(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{"/mnt", "/home"} {
		_, err := claudeSandboxConfigRoot([]string{"CLAUDE_CONFIG_DIR=" + dir}, "/tasks/current")
		if err == nil || !strings.Contains(err.Error(), "would widen the agent sandbox") {
			t.Fatalf("claudeSandboxConfigRoot(%q) error = %v, want sandbox widening rejection", dir, err)
		}
	}
}

func TestClaudeSandboxConfigRootRejectsAncestorOfTaskWorkspace(t *testing.T) {
	t.Parallel()

	root, err := resolveSandboxPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "task")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err = claudeSandboxConfigRoot([]string{"CLAUDE_CONFIG_DIR=" + root}, workspace)
	if err == nil || !strings.Contains(err.Error(), "contains the task workspace") {
		t.Fatalf("claudeSandboxConfigRoot() error = %v, want task workspace rejection", err)
	}
}
