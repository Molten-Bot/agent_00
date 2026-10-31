package agentruntime

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func claudeWritableRoots(t *testing.T, args []string) []string {
	t.Helper()
	const prefix = "sandbox_workspace_write.writable_roots="
	for _, arg := range args {
		if !strings.HasPrefix(arg, prefix) {
			continue
		}
		var roots []string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, prefix)), &roots); err != nil {
			t.Fatalf("decode writable roots from %q: %v", arg, err)
		}
		return roots
	}
	t.Fatalf("no writable roots argument in %v", args)
	return nil
}

// An implementation pass needs Claude's persistent configuration directory to
// be writable: the CLI creates session state there before it runs any shell
// command, and without it every Bash call fails with EROFS for the whole run.
func TestClaudeImplementationSandboxWritesToClaudeConfigDir(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	configDir := t.TempDir()
	resolvedWorkspace, err := resolveSandboxPath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolvedConfigDir, err := resolveSandboxPath(configDir)
	if err != nil {
		t.Fatal(err)
	}

	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := rt.BuildCommand(workspace, "fix it", RunOptions{
		WorkspaceDir: workspace,
		Env:          []string{"CLAUDE_CONFIG_DIR=" + configDir},
	})
	if err != nil {
		t.Fatal(err)
	}

	roots := claudeWritableRoots(t, cmd.Args)
	if !slices.Contains(roots, resolvedWorkspace) {
		t.Fatalf("writable roots %v must still contain the task workspace %q", roots, resolvedWorkspace)
	}
	if !slices.Contains(roots, resolvedConfigDir) {
		t.Fatalf("writable roots %v must contain the Claude config dir %q", roots, resolvedConfigDir)
	}
}

// Without an explicit CLAUDE_CONFIG_DIR the agent home is already task-local,
// so nothing outside the workspace may be added.
func TestClaudeSandboxKeepsWorkspaceOnlyWithoutClaudeConfigDir(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	resolvedWorkspace, err := resolveSandboxPath(workspace)
	if err != nil {
		t.Fatal(err)
	}

	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := rt.BuildCommand(workspace, "fix it", RunOptions{
		WorkspaceDir: workspace,
		Env:          []string{"TMPDIR=" + filepath.Join(workspace, "tmp")},
	})
	if err != nil {
		t.Fatal(err)
	}

	roots := claudeWritableRoots(t, cmd.Args)
	if len(roots) != 1 || roots[0] != resolvedWorkspace {
		t.Fatalf("writable roots = %v, want only %q", roots, resolvedWorkspace)
	}
}

// A read-only review pass must not run shell commands at all, so it keeps the
// read-only root profile and never gains write access to the config dir.
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
