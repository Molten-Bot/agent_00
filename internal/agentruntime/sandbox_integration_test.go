package agentruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

// Opt in on a host/container capable of creating Codex sandbox namespaces.
// No provider login, network access, or model invocation is required.
func TestAgentSandboxWriteBoundary(t *testing.T) {
	if os.Getenv("HARNESS_TEST_CODEX_SANDBOX") != "1" {
		t.Skip("set HARNESS_TEST_CODEX_SANDBOX=1 to exercise the installed sandbox")
	}
	for _, harness := range []string{HarnessCodex, HarnessClaude} {
		t.Run(harness, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "task")
			repo := filepath.Join(root, "repo")
			tmp := filepath.Join(root, "tmp")
			persistent := filepath.Join(base, "persistent")
			sibling := filepath.Join(base, "other-task")
			sandboxHome := filepath.Join(root, ".moltenhub-agent-io", "config", "sandbox")
			for _, dir := range []string{repo, tmp, persistent, sibling, sandboxHome} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// An operator config must not broaden the task's writable roots.
			if err := os.WriteFile(filepath.Join(persistent, "config.toml"), []byte("[sandbox_workspace_write]\nwritable_roots = [\"/tmp\"]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sibling, filepath.Join(repo, "escape")); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(root, "probe")
			if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
printf yes > repo-write
printf yes > "$TMPDIR/tmp-write"
for path in "$TEST_BASE/global-write" "$TEST_SIBLING/write" "$TEST_PERSISTENT/write" escape/write; do
    if (printf bad > "$path") 2>/dev/null; then
        echo "sandbox allowed an outside write" >&2
        exit 1
    fi
done
`), 0o700); err != nil {
				t.Fatal(err)
			}
			env := []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + tmp,
				"CODEX_HOME=" + persistent, "TEST_BASE=" + base,
				"TEST_SIBLING=" + sibling, "TEST_PERSISTENT=" + persistent,
			}
			rt, err := Resolve(harness, script)
			if err != nil {
				t.Fatal(err)
			}
			cmd, err := rt.BuildCommand(repo, "probe", RunOptions{WorkspaceDir: root, Env: env})
			if err != nil {
				t.Fatal(err)
			}
			if harness == HarnessCodex {
				// Exercise the generated Codex policy without invoking a model.
				cmd.Name = "codex"
				cmd.Args = append([]string{"sandbox", "-c", `sandbox_mode="workspace-write"`}, cmd.Args[3:]...)
				cmd.Args = append(cmd.Args, "--", script)
				cmd.Stdin = ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := (execx.OSRunner{}).Run(ctx, cmd)
			if err != nil {
				t.Fatalf("sandbox probe failed: %v\n%s\n%s", err, res.Stdout, res.Stderr)
			}
			for _, path := range []string{filepath.Join(repo, "repo-write"), filepath.Join(tmp, "tmp-write")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("expected allowed write %s: %v", path, err)
				}
			}
			for _, path := range []string{filepath.Join(base, "global-write"), filepath.Join(sibling, "write"), filepath.Join(persistent, "write")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("outside write was not denied: %s (%v)", path, err)
				}
			}
		})
	}
}
