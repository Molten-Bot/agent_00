package agentruntime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Claude writes its session-env directory, project state and rotated
// credentials into CLAUDE_CONFIG_DIR on every Bash tool call. That directory is
// deliberately persistent and shared across tasks, so the sandbox has to grant
// it write access; otherwise the CLI keeps running while every shell
// invocation fails with "EROFS: read-only file system".
func TestClaudeSandboxGrantsPersistentConfigDirWriteAccess(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configDir := t.TempDir()
	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}

	cmd, err := rt.BuildCommand(root, "fix it", RunOptions{
		WorkspaceDir: root,
		Env:          []string{"CLAUDE_CONFIG_DIR=" + configDir},
	})
	if err != nil {
		t.Fatal(err)
	}

	resolvedRoot, err := resolveSandboxPath(root)
	if err != nil {
		t.Fatal(err)
	}
	resolvedConfigDir, err := resolveSandboxPath(configDir)
	if err != nil {
		t.Fatal(err)
	}
	want := `sandbox_workspace_write.writable_roots=["` + resolvedRoot + `","` + resolvedConfigDir + `"]`
	if !slices.Contains(cmd.Args, want) {
		t.Fatalf("missing writable root %q in %v", want, cmd.Args)
	}
	// The config dir is a sandbox grant only; it must not widen the files
	// Claude itself is told it may edit.
	for i, arg := range cmd.Args {
		if arg == "--add-dir" && cmd.Args[i+1] == resolvedConfigDir {
			t.Fatalf("persistent config dir was added to Claude's edit roots: %v", cmd.Args)
		}
	}
}

func TestClaudeSandboxIgnoresUnusableConfigDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := resolveSandboxPath(root)
	if err != nil {
		t.Fatal(err)
	}
	onlyWorkspace := `sandbox_workspace_write.writable_roots=["` + resolvedRoot + `"]`

	for name, env := range map[string][]string{
		// The process environment is never consulted: a writable root outside
		// the task workspace must be a deliberate caller decision.
		"absent":       {"TMPDIR=/global/tmp"},
		"empty":        {"CLAUDE_CONFIG_DIR=  "},
		"missing path": {"CLAUDE_CONFIG_DIR=" + filepath.Join(root, "absent")},
		"regular file": {"CLAUDE_CONFIG_DIR=" + file},
	} {
		cmd, err := rt.BuildCommand(root, "fix it", RunOptions{WorkspaceDir: root, Env: env})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Contains(cmd.Args, onlyWorkspace) {
			t.Fatalf("%s: writable roots widened beyond the task workspace: %v", name, cmd.Args)
		}
	}
}

// Review runs stay read-only; they must not pick up the config-dir grant.
func TestClaudeReviewSandboxIgnoresConfigDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configDir := t.TempDir()
	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}

	cmd, err := rt.BuildCommand(root, "review", RunOptions{
		WorkspaceDir:    root,
		Env:             []string{"CLAUDE_CONFIG_DIR=" + configDir},
		DisallowedTools: []string{"Write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, configDir) {
			t.Fatalf("review sandbox referenced the persistent config dir: %v", cmd.Args)
		}
	}
}
