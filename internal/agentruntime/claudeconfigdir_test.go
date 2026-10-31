package agentruntime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Claude's transcript, todo and telemetry trees are shared by every task that
// uses the same persistent CLAUDE_CONFIG_DIR, so granting them would let one
// run's commands overwrite another run's state. Only the shell integration
// state Bash needs may become writable.
func TestClaudeSandboxKeepsSharedClaudeStateTreesReadOnly(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	resolvedConfigDir, err := resolveSandboxPath(configDir)
	if err != nil {
		t.Fatal(err)
	}

	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := rt.BuildCommand("/tmp/repo", "fix bug", RunOptions{
		Env: []string{"CLAUDE_CONFIG_DIR=" + configDir},
	})
	if err != nil {
		t.Fatal(err)
	}

	roots := sandboxWritableRoots(t, cmd.Args)
	for _, name := range []string{"projects", "todos", "statsig"} {
		dir := filepath.Join(resolvedConfigDir, name)
		if slices.Contains(roots, dir) {
			t.Fatalf("writable roots %v expose the cross-task Claude state tree %q", roots, dir)
		}
	}
}

// Claude's state directories are the one grant that leaves the task workspace,
// so they are confined to CLAUDE_CONFIG_DIR here instead. A symlinked state
// directory would otherwise make its target writable for the whole run.
func TestClaudeSandboxRejectsSymlinkedClaudeStateDir(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(configDir, claudeStateSubdirs[0])); err != nil {
		t.Fatal(err)
	}

	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.BuildCommand("/tmp/repo", "fix bug", RunOptions{
		Env: []string{"CLAUDE_CONFIG_DIR=" + configDir},
	})
	if err == nil || !strings.Contains(err.Error(), "must stay inside") {
		t.Fatalf("BuildCommand() error = %v, want redirected Claude state directory rejection", err)
	}
}

// A read-only review pass must not run shell commands at all, so it keeps the
// read-only root profile and never gains write access to the config dir, even
// when one is set in the agent environment.
func TestClaudeReviewSandboxKeepsClaudeConfigDirReadOnly(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	configDir := t.TempDir()
	resolvedConfigDir, err := resolveSandboxPath(configDir)
	if err != nil {
		t.Fatal(err)
	}

	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := rt.BuildCommand(workspace, "review it", RunOptions{
		WorkspaceDir:    workspace,
		Env:             []string{"CLAUDE_CONFIG_DIR=" + configDir},
		DisallowedTools: []string{"Bash"},
	})
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, resolvedConfigDir) {
		t.Fatalf("review sandbox %q must not grant the Claude config dir %q", joined, resolvedConfigDir)
	}
	if !strings.Contains(joined, `":root"="read"`) {
		t.Fatalf("review sandbox %q must stay read-only at the root", joined)
	}
}
