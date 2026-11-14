package agentruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestClaudeReviewSandboxNetworkAndReadOnly(t *testing.T) {
	if os.Getenv("HARNESS_TEST_CODEX_SANDBOX") != "1" {
		t.Skip("set HARNESS_TEST_CODEX_SANDBOX=1 to exercise the installed sandbox")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	home := filepath.Join(root, ".agent00-agent-io", "config", "sandbox")
	tmp := filepath.Join(root, ".agent00-agent-io", "tmp")
	for _, dir := range []string{repo, home, tmp} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("network-ok"))
	}))
	defer server.Close()
	script := filepath.Join(root, "review-probe")
	if err := os.WriteFile(script, []byte(`#!/usr/bin/env python3
import errno, os, urllib.request
assert urllib.request.urlopen(os.environ["PROBE_URL"], timeout=5).read() == b"network-ok"
with open(os.environ["TMPDIR"] + "/tmp-write", "w") as f:
    f.write("allowed runtime IO")
for path in ["repo-write", ".git/config", os.environ["TEST_ROOT"] + "/outside-write", "/tmp/agent00-review-write"]:
    try:
        with open(path, "w") as f:
            f.write("unexpected write")
    except OSError as error:
        if error.errno in (errno.EACCES, errno.EPERM, errno.EROFS):
            continue
        raise
    raise SystemExit("review sandbox allowed write: " + path)
`), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	rt, err := Resolve(HarnessClaude, script)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := rt.BuildCommand(repo, "review", RunOptions{
		WorkspaceDir: root, DisallowedTools: []string{"Edit", "Write", "NotebookEdit", "Bash"},
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + tmp, "TEST_ROOT=" + root, "PROBE_URL=" + server.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := (execx.OSRunner{}).Run(ctx, cmd)
	if err != nil {
		t.Fatalf("review sandbox probe failed: %v\n%s\n%s", err, res.Stdout, res.Stderr)
	}
}

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
			sandboxHome := filepath.Join(root, ".agent00-agent-io", "config", "sandbox")
			for _, dir := range []string{repo, filepath.Join(repo, ".git", "objects"), filepath.Join(repo, ".git", "refs", "heads"), tmp, persistent, sibling, sandboxHome} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core]\n\trepositoryformatversion = 0\n\tbare = false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
				t.Fatal(err)
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
printf yes > .git/index.lock
if [ "$ALLOW_PERSISTENT_WRITE" = yes ]; then
    printf yes > "$TEST_PERSISTENT/session-write"
elif (printf bad > "$TEST_PERSISTENT/session-write") 2>/dev/null; then
    echo "sandbox allowed persistent config write" >&2
    exit 1
fi
if (printf bad > .git/config) 2>/dev/null; then
    echo "sandbox allowed Git config write" >&2
    exit 1
fi
printf yes > "$TMPDIR/tmp-write"
for path in "$TEST_BASE/global-write" "$TEST_SIBLING/write" escape/write; do
    if (printf bad > "$path") 2>/dev/null; then
        echo "sandbox allowed an outside write: $path" >&2
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
			if harness == HarnessCodex {
				env = append(env, "ALLOW_PERSISTENT_WRITE=yes")
			} else {
				env = append(env, "ALLOW_PERSISTENT_WRITE=no")
			}
			rt, err := Resolve(harness, script)
			if err != nil {
				t.Fatal(err)
			}
			opts := RunOptions{WorkspaceDir: root, Env: env}
			cmd, err := rt.BuildCommand(repo, "probe", opts)
			if err != nil {
				t.Fatal(err)
			}
			if harness == HarnessCodex {
				// Exercise the generated Codex policy without invoking a model.
				cmd.Args = []string{"sandbox", "-P", taskPermissionProfile, "-c", taskPermissionProfileConfig(repo, opts, persistent), "--", script}
				cmd.Stdin = ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := (execx.OSRunner{}).Run(ctx, cmd)
			if err != nil {
				t.Fatalf("sandbox probe failed: %v\n%s\n%s", err, res.Stdout, res.Stderr)
			}
			expectedWrites := []string{filepath.Join(repo, "repo-write"), filepath.Join(repo, ".git", "index.lock"), filepath.Join(tmp, "tmp-write")}
			if harness == HarnessCodex {
				expectedWrites = append(expectedWrites, filepath.Join(persistent, "session-write"))
			}
			for _, path := range expectedWrites {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("expected allowed write %s: %v", path, err)
				}
			}
			deniedWrites := []string{filepath.Join(base, "global-write"), filepath.Join(sibling, "write")}
			if harness != HarnessCodex {
				deniedWrites = append(deniedWrites, filepath.Join(persistent, "session-write"))
			}
			for _, path := range deniedWrites {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("outside write was not denied: %s (%v)", path, err)
				}
			}
		})
	}
}
