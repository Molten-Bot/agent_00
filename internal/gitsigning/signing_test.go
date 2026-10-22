package gitsigning

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"golang.org/x/crypto/ssh"
)

func testKey(t *testing.T, kind string, encrypted bool) string {
	t.Helper()
	var key any
	var err error
	switch kind {
	case "rsa":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	case "ecdsa":
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		_, key, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if encrypted {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(key, "test", []byte("test-passphrase"))
	} else {
		block, err = ssh.MarshalPrivateKey(key, "test")
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

func TestImportKeyFormats(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"ed25519", "rsa", "ecdsa"} {
		for _, encrypted := range []bool{false, true} {
			name := kind
			if encrypted {
				name += "-encrypted"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				dir := filepath.Join(t.TempDir(), "signing")
				raw := testKey(t, kind, encrypted)
				cfg, err := Import(dir, raw, "test-passphrase")
				if err != nil {
					t.Fatal(err)
				}
				if cfg.PublicKey == "" || !strings.HasPrefix(cfg.Fingerprint, "SHA256:") {
					t.Fatalf("missing public metadata: %+v", cfg)
				}
				for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, cfg.KeyID): 0600} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != mode {
						t.Fatalf("permissions %s: %v %v", path, info, err)
					}
				}
				stored, err := os.ReadFile(filepath.Join(dir, cfg.KeyID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ssh.ParsePrivateKey(stored); err != nil {
					t.Fatalf("stored key is not unlocked: %v", err)
				}
				again, err := Import(dir, raw, "test-passphrase")
				if err != nil || again.KeyID != cfg.KeyID {
					t.Fatalf("repeat import: %v", err)
				}
				cfg.Name, cfg.Account, cfg.Email = "Test Bot", "test-bot", "123+test-bot@users.noreply.github.com"
				snapshot, err := Load(dir, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := snapshot.Test(context.Background(), execx.OSRunner{}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestImportErrorsDoNotExposeSecrets(t *testing.T) {
	t.Parallel()
	encrypted := testKey(t, "ed25519", true)
	for _, test := range []struct{ key, pass string }{{"secret-invalid-key", "secret-passphrase"}, {encrypted, "wrong-secret"}, {encrypted, ""}} {
		_, err := Import(t.TempDir(), test.key, test.pass)
		if !errors.Is(err, ErrSetup) {
			t.Fatalf("expected setup error, got %v", err)
		}
		if strings.Contains(err.Error(), test.key) || (test.pass != "" && strings.Contains(err.Error(), test.pass)) {
			t.Fatal("secret leaked")
		}
	}
}

func TestVerifyNewCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	cfg, err := Import(filepath.Join(dir, "keys"), testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name, cfg.Account, cfg.Email = "Test Bot", "test-bot", "123+test-bot@users.noreply.github.com"
	snapshot, err := Load(filepath.Join(dir, "keys"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	runner := execx.OSRunner{}
	run := func(args ...string) string {
		t.Helper()
		res, err := runner.Run(ctx, execx.Command{Name: "git", Dir: repo, Env: snapshot.Environment(nil), Args: args})
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}
	run("init", "--quiet")
	run("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "existing unsigned history")
	baseline := run("rev-parse", "HEAD")
	run("commit", "--allow-empty", "-m", "signed task commit")
	if err := snapshot.VerifyNew(ctx, runner, repo, []string{baseline}); err != nil {
		t.Fatal(err)
	}
	run("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unsigned task commit")
	if err := snapshot.VerifyNew(ctx, runner, repo, []string{baseline}); !errors.Is(err, ErrSetup) {
		t.Fatalf("unsigned accepted: %v", err)
	}
	run("commit", "--amend", "--allow-empty", "--no-edit", "-S")
	if err := snapshot.VerifyNew(ctx, runner, repo, []string{baseline}); err != nil {
		t.Fatal(err)
	}
	// A different signed key is also rejected.
	other, err := Import(filepath.Join(dir, "keys"), testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	run("-c", "user.signingkey="+filepath.Join(dir, "keys", other.KeyID), "commit", "--allow-empty", "-m", "wrong signer")
	if err := snapshot.VerifyNew(ctx, runner, repo, []string{baseline}); !errors.Is(err, ErrSetup) {
		t.Fatalf("wrong signer accepted: %v", err)
	}
}

func TestEnvironmentPreservesIsolationAndGitConfig(t *testing.T) {
	t.Parallel()
	snapshot := &Snapshot{Config: Config{Name: "Test", Email: "test@example.com"}, KeyPath: "/managed/key", AllowedSignersPath: "/managed/allowed"}
	env := []string{"HOME=/task/home", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=helper", "GIT_COMMITTER_EMAIL=old@example.com"}
	result := strings.Join(snapshot.Environment(env), "\n")
	for _, value := range []string{"HOME=/task/home", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=helper", "GIT_CONFIG_COUNT=8", "GIT_COMMITTER_EMAIL=test@example.com"} {
		if !strings.Contains(result, value) {
			t.Errorf("missing %s", value)
		}
	}
	if strings.Contains(result, "old@example.com") || env[1] != "GIT_CONFIG_COUNT=1" {
		t.Fatal("environment not isolated")
	}
}

func TestLoadRejectsTampering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Import(dir, testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Account, cfg.Name, cfg.Email = "test-bot", "Test Bot", "bot@example.com"
	if err := os.Chmod(filepath.Join(dir, cfg.KeyID), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) {
		t.Fatalf("open permissions accepted: %v", err)
	}
	cfg.KeyID = "../../outside"
	if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) {
		t.Fatalf("path traversal accepted: %v", err)
	}
}
