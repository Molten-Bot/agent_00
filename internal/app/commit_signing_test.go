package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"golang.org/x/crypto/ssh"
)

func newSigningTestHarness(t *testing.T) Harness {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "signing")
	cfg, err := gitsigning.Import(dir, string(pem.EncodeToMemory(block)), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name, cfg.Account, cfg.Email = "Test Bot", "test-bot", "123+test-bot@users.noreply.github.com"
	snapshot, err := gitsigning.Load(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(execx.OSRunner{})
	h.Signing = snapshot
	h.signingHistory = map[string]map[string]bool{}
	return h
}

func signingTestGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	res, err := (execx.OSRunner{}).Run(context.Background(), execx.Command{Name: "git", Dir: dir, Env: env, Args: args})
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

func TestSigningCoversHarnessAgentMergeAndPush(t *testing.T) {
	t.Parallel()
	h := newSigningTestHarness(t)
	ctx := context.Background()
	root := t.TempDir()
	env := h.Signing.Environment(nil)
	remote := filepath.Join(root, "origin.git")
	signingTestGit(t, root, env, "init", "--bare", "--initial-branch=main", remote)
	seed := filepath.Join(root, "seed")
	signingTestGit(t, root, env, "clone", remote, seed)
	signingTestGit(t, seed, env, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "existing unsigned root")
	signingTestGit(t, seed, env, "push", "origin", "main")
	repo := filepath.Join(root, "task")
	run := func(cmd execx.Command) string {
		t.Helper()
		res, err := h.runCommand(ctx, "git", cmd)
		if err != nil {
			t.Fatalf("%v: %v", cmd.Args, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	run(execx.Command{Name: "git", Args: []string{"clone", remote, repo}})
	if err := os.WriteFile(filepath.Join(repo, "task.txt"), []byte("task\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(addCommand(repo))
	run(commitCommand(repo, "normal task"))
	// Model subprocesses keep signing even with an isolated HOME and existing HTTPS config.
	agentEnv, err := prepareAgentIOEnv(root, environWithOverrides(os.Environ(), "HOME="+filepath.Join(root, "home"), "CODEX_HOME="+filepath.Join(root, "codex")))
	if err != nil {
		t.Fatal(err)
	}
	agentEnv = h.Signing.Environment(agentEnv)
	signingTestGit(t, repo, agentEnv, "commit", "--allow-empty", "-m", "agent-created commit")
	run(execx.Command{Name: "git", Dir: repo, Args: []string{"commit", "--allow-empty", "-m", "remediation"}})
	// Fetch and merge pre-existing unsigned remote work; only the new merge must be signed.
	if err := os.WriteFile(filepath.Join(seed, "upstream.txt"), []byte("upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	signingTestGit(t, seed, env, "add", ".")
	signingTestGit(t, seed, env, "-c", "commit.gpgsign=false", "commit", "-m", "external unsigned work")
	signingTestGit(t, seed, env, "push", "origin", "main")
	run(fetchBranchCommand(repo, "main"))
	run(mergeFetchedBranchCommand(repo))
	run(pushCommand(repo, "main"))
	signedHead := signingTestGit(t, repo, env, "rev-parse", "HEAD")
	if remoteHead := signingTestGit(t, remote, env, "rev-parse", "refs/heads/main"); remoteHead != signedHead {
		t.Fatal("signed work was not pushed")
	}
	signingTestGit(t, repo, env, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unsigned new task commit")
	// A locally modified remote-tracking ref must not exempt new work after a fetch.
	signingTestGit(t, repo, env, "update-ref", "refs/remotes/origin/local-only", "HEAD")
	run(fetchBranchCommand(repo, "main"))
	if _, err := h.runCommand(ctx, "git", pushCommand(repo, "main")); !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("unsigned push not blocked: %v", err)
	}
	if remoteHead := signingTestGit(t, remote, env, "rev-parse", "refs/heads/main"); remoteHead != signedHead {
		t.Fatal("blocked push changed remote")
	}
}

func TestSigningBootstrapEmptyRepository(t *testing.T) {
	t.Parallel()
	h := newSigningTestHarness(t)
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	signingTestGit(t, root, h.Signing.Environment(nil), "init", "--bare", remote)
	repo := repoWorkspace{Dir: filepath.Join(root, "task"), URL: remote, RelDir: "task"}
	if err := h.bootstrapUninitializedMainBranch(context.Background(), repo, remote); err != nil {
		t.Fatal(err)
	}
	env := h.Signing.Environment(nil)
	signingTestGit(t, repo.Dir, env, "verify-commit", "HEAD")
	if email := signingTestGit(t, repo.Dir, env, "show", "-s", "--format=%ae|%ce", "HEAD"); email != h.Signing.Config.Email+"|"+h.Signing.Config.Email {
		t.Fatalf("bootstrap identity: %s", email)
	}
}
