package agentruntime

import (
	"fmt"
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

// A review run writes session state and rotates credentials in
// CLAUDE_CONFIG_DIR exactly like an implementation run, so it needs the same
// grant. Everything outside that directory and the harness IO dir - the
// repository included - must stay read-only.
func TestClaudeReviewSandboxGrantsConfigDirWriteAccessOnly(t *testing.T) {
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
	resolvedRoot, err := resolveSandboxPath(root)
	if err != nil {
		t.Fatal(err)
	}
	resolvedConfigDir, err := resolveSandboxPath(configDir)
	if err != nil {
		t.Fatal(err)
	}

	permissions := ""
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "permissions.agent00_review=") {
			permissions = arg
		}
	}
	if permissions == "" {
		t.Fatalf("review sandbox did not configure a permissions profile: %v", cmd.Args)
	}
	if !strings.Contains(permissions, `":root"="read"`) {
		t.Fatalf("review sandbox dropped the read-only root: %q", permissions)
	}
	if want := fmt.Sprintf("%q=\"write\"", resolvedConfigDir); !strings.Contains(permissions, want) {
		t.Fatalf("review sandbox missing config dir grant %q: %q", want, permissions)
	}
	if write := fmt.Sprintf("%q=\"write\"", resolvedRoot); strings.Contains(permissions, write) {
		t.Fatalf("review sandbox granted write access to the task workspace: %q", permissions)
	}
	// The grant is a sandbox root only; Claude's own edit roots stay untouched.
	for i, arg := range cmd.Args {
		if arg == "--add-dir" && cmd.Args[i+1] == resolvedConfigDir {
			t.Fatalf("persistent config dir was added to Claude's edit roots: %v", cmd.Args)
		}
	}
}

// A CLAUDE_CONFIG_DIR that spans the filesystem root, the home directory or the
// task workspace itself must fail the build instead of silently widening the
// sandbox. validateWorkspaceDirs cannot catch these: the grant is outside the
// workspace by design.
func TestClaudeSandboxRejectsOverlyBroadConfigDir(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		configDir string
	}{
		{name: "filesystem root", configDir: string(filepath.Separator)},
		{name: "workspace ancestor", configDir: filepath.Dir(root)},
		{name: "workspace itself", configDir: root},
	}
	if home, err := os.UserHomeDir(); err == nil && sandboxTestDirExists(home) {
		cases = append(cases, struct {
			name      string
			configDir string
		}{name: "home directory", configDir: home})
	}

	for _, tc := range cases {
		for _, disallowed := range [][]string{nil, {"Write"}} {
			_, err := rt.BuildCommand(root, "fix it", RunOptions{
				WorkspaceDir:    root,
				Env:             []string{"CLAUDE_CONFIG_DIR=" + tc.configDir},
				DisallowedTools: disallowed,
			})
			if err == nil {
				t.Fatalf("%s (disallowed=%v): build accepted an overly broad CLAUDE_CONFIG_DIR %q", tc.name, disallowed, tc.configDir)
			}
			if !strings.Contains(err.Error(), "widen the agent sandbox") {
				t.Fatalf("%s (disallowed=%v): unexpected error %v", tc.name, disallowed, err)
			}
		}
	}
}

func sandboxTestDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
