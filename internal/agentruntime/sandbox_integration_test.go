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
for path in ["repo-write", ".git/config", os.environ["TEST_ROOT"] + "/outside-write"]:
    try:
        with open(path, "w") as f:
            f.write("unexpected write")
    except OSError as error:
        if error.errno in (errno.EACCES, errno.EPERM, errno.EROFS):
            continue
        raise
    raise SystemExit("review sandbox allowed write: " + path)
# Some sandbox versions provide a private, disposable /tmp. A write there
# is safe only if it cannot appear in the host's /tmp (checked below).
try:
    with open(os.environ["PROBE_GLOBAL_PATH"], "w") as f:
        f.write("private sandbox scratch")
except OSError as error:
    if error.errno not in (errno.EACCES, errno.EPERM, errno.EROFS):
        raise
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
	globalPath := filepath.Join("/tmp", filepath.Base(filepath.Dir(root))+"-review-write")
	t.Cleanup(func() { _ = os.Remove(globalPath) })
	cmd, err := rt.BuildCommand(repo, "review", RunOptions{
		WorkspaceDir: root, DisallowedTools: []string{"Edit", "Write", "NotebookEdit", "Bash"},
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + tmp, "TEST_ROOT=" + root, "PROBE_URL=" + server.URL, "PROBE_GLOBAL_PATH=" + globalPath},
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
	if _, err := os.Stat(globalPath); !os.IsNotExist(err) {
		t.Fatalf("review wrote outside its workspace into host /tmp: %v", err)
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

func TestCodexReadOnlyReviewCanReadDiskWorkspace(t *testing.T) {
	if os.Getenv("HARNESS_TEST_CODEX_SANDBOX") != "1" {
		t.Skip("set HARNESS_TEST_CODEX_SANDBOX=1 to exercise the installed sandbox")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	home := filepath.Join(root, "codex-home")
	for _, dir := range []string{repo, home} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "marker"), []byte("prepared checkout"), 0600); err != nil {
		t.Fatal(err)
	}
	rt, err := Resolve(HarnessCodex, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := rt.BuildCommand(repo, "Review", RunOptions{
		WorkspaceDir: root, DisallowedTools: []string{"Edit", "Write"},
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CODEX_HOME=" + home},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Parse the actual exec invocation too: sandbox accepts flags (notably -P)
	// that exec does not. --help avoids authentication and a model request.
	parseCmd := cmd
	parseCmd.Args = append(append([]string(nil), cmd.Args...), "--help")
	parseCmd.Stdin = ""
	parseCtx, parseCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer parseCancel()
	if res, err := (execx.OSRunner{}).Run(parseCtx, parseCmd); err != nil {
		t.Fatalf("Codex exec rejected review arguments: %v\n%s\n%s", err, res.Stdout, res.Stderr)
	}
	// Exercise the generated review policy without running a model or needing auth.
	cmd.Args[0] = "sandbox"
	cmd.Args = append(cmd.Args, "--", "/bin/sh", "-c", `set -eu
 test "$(cat marker)" = "prepared checkout"
 if (printf changed > marker) 2>/dev/null; then exit 1; fi
 if (printf created > new-file) 2>/dev/null; then exit 1; fi
 `)
	cmd.Stdin = ""
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := (execx.OSRunner{}).Run(ctx, cmd)
	if err != nil {
		t.Fatalf("read-only review probe: %v\n%s\n%s", err, res.Stdout, res.Stderr)
	}
}

// Run with TMPDIR on /dev/shm to exercise RAM-backed task workspaces as well
// as the default disk-backed temporary directory.
func TestReviewSandboxWorkspaceReadable(t *testing.T) {
	if os.Getenv("HARNESS_TEST_CODEX_SANDBOX") != "1" {
		t.Skip("set HARNESS_TEST_CODEX_SANDBOX=1 to exercise the installed sandbox")
	}
	for _, harness := range []string{HarnessCodex, HarnessClaude} {
		t.Run(harness, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			for _, dir := range []string{filepath.Join(repo, ".git"), filepath.Join(root, ".agent00-agent-io", "config", "sandbox")} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{filepath.Join(root, "AGENTS.md"), filepath.Join(repo, ".git", "HEAD"), filepath.Join(repo, "guide.md")} {
				if err := os.WriteFile(path, []byte("prepared workspace\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := filepath.Join(root, "probe")
			if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
test "$PWD" = "$TEST_ROOT/repo"
for path in ../AGENTS.md .git/HEAD guide.md; do
    test "$(cat "$path")" = 'prepared workspace'
done
for path in repo-write .git/HEAD guide.md ../AGENTS.md ../outside-write; do
    if (printf bad > "$path") 2>/dev/null; then
        echo "review allowed write: $path" >&2
        exit 1
    fi
done
`), 0700); err != nil {
				t.Fatal(err)
			}
			rt, _ := Resolve(harness, script)
			cmd, err := rt.BuildCommand(repo, "review", RunOptions{
				WorkspaceDir: root, DisallowedTools: []string{"Write"},
				Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TEST_ROOT=" + root,
					"CODEX_HOME=" + filepath.Join(root, ".agent00-agent-io", "config", "sandbox")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if harness == HarnessCodex {
				cmd.Name = "codex"
				cmd.Args[0] = "sandbox"
				cmd.Args = append(cmd.Args, "--", script)
			}
			cmd.Stdin = ""
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := (execx.OSRunner{}).Run(ctx, cmd)
			if err != nil {
				t.Fatalf("review workspace probe failed: %v\n%s\n%s", err, res.Stdout, res.Stderr)
			}
		})
	}
}
