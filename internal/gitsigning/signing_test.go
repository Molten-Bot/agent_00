package gitsigning

import (
	"context"
	"crypto/dsa" //nolint:staticcheck // deliberately deprecated: only used to produce an unsupported key type for tests.
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
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

// fakeRunner is a hermetic execx.Runner stub that lets tests force specific
// git subcommands to fail without touching a real repository or network.
type fakeRunner struct {
	fail func(args []string) bool
}

func (f fakeRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	if f.fail != nil && f.fail(cmd.Args) {
		return execx.Result{}, fmt.Errorf("fake failure for %v", cmd.Args)
	}
	return execx.Result{}, nil
}

// dsaPrivateKeyPEM returns a legacy DSA private key PEM block. DSA is parsed
// successfully by ssh.ParseRawPrivateKey but is not one of the key types
// Import accepts, so it is the only practical way to exercise Import's
// "unsupported key type" branch without a fake/mocked key parser.
func dsaPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	var params dsa.Parameters
	if err := dsa.GenerateParameters(&params, rand.Reader, dsa.L1024N160); err != nil {
		t.Fatal(err)
	}
	var priv dsa.PrivateKey
	priv.Parameters = params
	if err := dsa.GenerateKey(&priv, rand.Reader); err != nil {
		t.Fatal(err)
	}
	type dsaKey struct {
		Version            int
		P, Q, G, Pub, Priv *big.Int
	}
	der, err := asn1.Marshal(dsaKey{0, priv.P, priv.Q, priv.G, priv.Y, priv.X})
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "DSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

func TestImportRejectsUnsupportedKeyType(t *testing.T) {
	t.Parallel()
	_, err := Import(t.TempDir(), dsaPrivateKeyPEM(t), "")
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "use an Ed25519, RSA, or ECDSA private key") {
		t.Fatalf("Import(dsa key) error = %v, want unsupported key type error", err)
	}
}

func TestImportFailsWhenSigningDirCannotBeCreated(t *testing.T) {
	t.Parallel()
	// A regular file in place of a parent directory component makes
	// os.MkdirAll fail, which should surface through Import via secureDir.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "signing")
	_, err := Import(dir, testKey(t, "ed25519", false), "")
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot create signing directory") {
		t.Fatalf("Import(blocked dir) error = %v, want cannot create signing directory error", err)
	}
}

func TestSecureDirCreatesRestrictedDirectoryAndIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "signing")
	if err := secureDir(dir); err != nil {
		t.Fatalf("secureDir() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("secureDir() permissions = %v %v, want 0700", info, err)
	}
	// Calling it again on the same directory must succeed unchanged.
	if err := secureDir(dir); err != nil {
		t.Fatalf("secureDir() repeat call error = %v", err)
	}
}

func TestSecureDirRejectsSymlinkDirectory(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := secureDir(link); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "invalid signing directory") {
		t.Fatalf("secureDir(symlink to dir) error = %v, want invalid signing directory error", err)
	}
}

func TestSecureDirRejectsBrokenSymlink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	link := filepath.Join(base, "broken-link")
	if err := os.Symlink(filepath.Join(base, "does-not-exist"), link); err != nil {
		t.Fatal(err)
	}
	if err := secureDir(link); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot create signing directory") {
		t.Fatalf("secureDir(broken symlink) error = %v, want cannot create signing directory error", err)
	}
}

func TestWriteImmutableFailsWhenParentDirMissing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing-dir", "file")
	if err := writeImmutable(path, []byte("data")); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot store signing material") {
		t.Fatalf("writeImmutable(missing parent) error = %v, want cannot store signing material error", err)
	}
}

func TestWriteImmutableRejectsNonRegularExistingPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeImmutable(path, []byte("data")); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "invalid existing signing file") {
		t.Fatalf("writeImmutable(existing directory) error = %v, want invalid existing signing file error", err)
	}
}

func TestWriteImmutableRejectsWrongPermissionExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod explicitly: the test process umask can otherwise mask the
	// requested WriteFile mode down to 0600, hiding the branch under test.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeImmutable(path, []byte("data")); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "invalid existing signing file") {
		t.Fatalf("writeImmutable(wrong permission) error = %v, want invalid existing signing file error", err)
	}
}

func TestWriteImmutableDetectsAllowedFileConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "signer.allowed")
	if err := writeImmutable(path, []byte("first@example.com key-one\n")); err != nil {
		t.Fatalf("writeImmutable(first write) error = %v", err)
	}
	// Re-writing identical content is idempotent and must not error.
	if err := writeImmutable(path, []byte("first@example.com key-one\n")); err != nil {
		t.Fatalf("writeImmutable(identical rewrite) error = %v", err)
	}
	if err := writeImmutable(path, []byte("second@example.com key-two\n")); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "signing metadata conflict") {
		t.Fatalf("writeImmutable(conflicting allowed content) error = %v, want signing metadata conflict error", err)
	}
}

func TestLoadRejectsCorruptKeyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("not a real key"))
	keyID := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(dir, keyID), []byte("not a valid ssh private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{KeyID: keyID, Account: "test-bot", Email: "bot@example.com"}
	if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot load signing key") {
		t.Fatalf("Load(corrupt key) error = %v, want cannot load signing key error", err)
	}
}

func TestLoadFailsWhenKeyFileCannotBeRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Import(dir, testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Account, cfg.Name, cfg.Email = "test-bot", "Test Bot", "bot@example.com"

	keyPath := filepath.Join(dir, cfg.KeyID)
	// 0000 keeps the group/other bits clear (passing the earlier permission
	// gate) while removing owner read access, so os.ReadFile itself fails.
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(keyPath, 0o600) })

	if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot read signing key") {
		t.Fatalf("Load(unreadable key file) error = %v, want cannot read signing key error", err)
	}
}

func TestImportFailsWhenKeyFileCannotBeWritten(t *testing.T) {
	t.Parallel()
	raw := testKey(t, "ed25519", false)

	// Discover the deterministic KeyID (sha256 of the public key) that
	// Import will compute for this raw key, so a conflicting directory
	// can be planted at that exact path ahead of time.
	probe, err := Import(t.TempDir(), raw, "")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, probe.KeyID), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := Import(dir, raw, ""); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "invalid existing signing file") {
		t.Fatalf("Import(conflicting key path) error = %v, want invalid existing signing file error", err)
	}
}

func TestEnvironmentNormalizesNegativeConfigCount(t *testing.T) {
	t.Parallel()
	snapshot := &Snapshot{Config: Config{Name: "Test", Email: "test@example.com"}, KeyPath: "/managed/key", AllowedSignersPath: "/managed/allowed"}
	env := []string{"GIT_CONFIG_COUNT=-3"}
	result := strings.Join(snapshot.Environment(env), "\n")
	// A negative starting count must be treated as zero, so the appended
	// pairs start at index 0 and the final count equals just the pair count.
	if !strings.Contains(result, "GIT_CONFIG_COUNT=7") {
		t.Fatalf("Environment(negative GIT_CONFIG_COUNT) = %q, want normalized count of 7", result)
	}
	if !strings.Contains(result, "GIT_CONFIG_KEY_0=gpg.format") {
		t.Fatalf("Environment(negative GIT_CONFIG_COUNT) = %q, want pairs starting at index 0", result)
	}
}

func TestLoadRejectsPublicKeyMetadataMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Import(dir, testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := Import(t.TempDir(), testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Account, cfg.Name, cfg.Email = "test-bot", "Test Bot", "bot@example.com"
	cfg.PublicKey = other.PublicKey
	if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "does not match saved metadata") {
		t.Fatalf("Load(public key mismatch) error = %v, want metadata mismatch error", err)
	}
}

func TestLoadRejectsMissingSigningAccountIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Import(dir, testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Account, cfg.Name = "test-bot", "Test Bot"

	for _, email := range []string{"", "has space@example.com"} {
		cfg.Email = email
		if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "signing account is missing") {
			t.Fatalf("Load(email %q) error = %v, want signing account missing error", email, err)
		}
	}

	cfg.Email, cfg.Account = "bot@example.com", ""
	if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "signing account is missing") {
		t.Fatalf("Load(empty account) error = %v, want signing account missing error", err)
	}
}

func TestLoadFailsWhenAllowedSignersFileCannotBeWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Import(dir, testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Account, cfg.Name, cfg.Email = "test-bot", "Test Bot", "bot@example.com"

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := Load(dir, cfg); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot store signing material") {
		t.Fatalf("Load(read-only dir) error = %v, want cannot store signing material error", err)
	}
}

func TestEnvironmentNilReceiverReturnsInputUnchanged(t *testing.T) {
	t.Parallel()
	var s *Snapshot
	env := []string{"HOME=/task/home", "GIT_CONFIG_COUNT=0"}
	got := s.Environment(env)
	if len(got) != len(env) {
		t.Fatalf("Environment(nil snapshot) = %#v, want unchanged %#v", got, env)
	}
	for i := range env {
		if got[i] != env[i] {
			t.Fatalf("Environment(nil snapshot)[%d] = %q, want %q", i, got[i], env[i])
		}
	}
}

func TestTestFailsWhenTempDirIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Import(dir, testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name, cfg.Account, cfg.Email = "Test Bot", "test-bot", "123+test-bot@users.noreply.github.com"
	snapshot, err := Load(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := snapshot.Test(context.Background(), execx.OSRunner{}); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot prepare signing check") {
		t.Fatalf("Test(unavailable TMPDIR) error = %v, want cannot prepare signing check error", err)
	}
}

func TestTestFailsWhenGitCommandFails(t *testing.T) {
	t.Parallel()
	snapshot := &Snapshot{Config: Config{Name: "Test Bot", Email: "bot@example.com"}, KeyPath: "/managed/key", AllowedSignersPath: "/managed/allowed"}
	runner := fakeRunner{fail: func(args []string) bool {
		return len(args) > 0 && args[0] == "verify-commit"
	}}
	err := snapshot.Test(context.Background(), runner)
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "local signing check failed; Git 2.34+ and ssh-keygen are required") {
		t.Fatalf("Test(failing verify-commit) error = %v, want local signing check failed error", err)
	}
}

func TestVerifyNewFailsWhenCommitEnumerationFails(t *testing.T) {
	t.Parallel()
	snapshot := &Snapshot{Config: Config{Name: "Test Bot", Email: "bot@example.com"}, KeyPath: "/managed/key", AllowedSignersPath: "/managed/allowed"}
	runner := fakeRunner{fail: func(args []string) bool {
		return len(args) > 0 && args[0] == "rev-list"
	}}
	err := snapshot.VerifyNew(context.Background(), runner, "/some/repo", nil)
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot enumerate commits before push") {
		t.Fatalf("VerifyNew(failing rev-list) error = %v, want cannot enumerate commits error", err)
	}
}

func TestVerifyNewRejectsMismatchedCommitterIdentity(t *testing.T) {
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
	if err := os.Mkdir(repo, 0o700); err != nil {
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
	run("commit", "--allow-empty", "-m", "properly signed and identified")
	first := run("rev-parse", "HEAD")
	if err := snapshot.VerifyNew(ctx, runner, repo, nil); err != nil {
		t.Fatalf("VerifyNew(valid commit) error = %v", err)
	}
	// Override GIT_COMMITTER_EMAIL directly: it takes precedence over
	// user.email for the committer identity, so the signature stays valid
	// for the managed key while the recorded committer identity diverges
	// from the saved metadata.
	spoofedEnv := append([]string(nil), snapshot.Environment(nil)...)
	for i, kv := range spoofedEnv {
		if strings.HasPrefix(kv, "GIT_COMMITTER_EMAIL=") {
			spoofedEnv[i] = "GIT_COMMITTER_EMAIL=someone-else@example.com"
		}
	}
	if _, err := runner.Run(ctx, execx.Command{Name: "git", Dir: repo, Env: spoofedEnv, Args: []string{"commit", "--allow-empty", "-m", "signed but different committer"}}); err != nil {
		t.Fatalf("git commit (spoofed committer): %v", err)
	}
	if err := snapshot.VerifyNew(ctx, runner, repo, []string{first}); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "has a different committer identity") {
		t.Fatalf("VerifyNew(mismatched committer) error = %v, want committer identity error", err)
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
