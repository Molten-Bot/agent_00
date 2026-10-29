package localruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"golang.org/x/crypto/ssh"
)

// failingRunner simulates a broken local git/ssh-keygen toolchain so tests can
// exercise SigningSettings.check's snapshot.Test error branch without
// depending on the host's actual git installation.
type failingRunner struct{}

func (failingRunner) Run(context.Context, execx.Command) (execx.Result, error) {
	return execx.Result{}, errors.New("git unavailable in test sandbox")
}

func TestSigningSettingsPathAndGitHubDefaults(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")

	var s SigningSettings
	if got, want := s.path(), defaultRuntimeConfigPath(); got != want {
		t.Fatalf("path() = %q, want default %q", got, want)
	}

	if got := s.github(); got != (gitsigning.GitHub{}) {
		t.Fatalf("github() = %+v, want zero value when GitHub is nil", got)
	}
}

func TestSigningSettingsSavedConfigReadErrorNotMissing(t *testing.T) {
	t.Parallel()
	// Pointing Path at a directory makes os.ReadFile fail with an error other
	// than os.ErrNotExist, exercising the generic read-failure branch.
	store := SigningSettings{Path: t.TempDir()}
	_, err := store.savedConfig()
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("savedConfig(directory path) error = %v, want wrapped ErrSetup", err)
	}
	if !strings.Contains(err.Error(), "cannot read runtime configuration") {
		t.Fatalf("savedConfig() error = %v, want cannot read runtime configuration", err)
	}
}

func TestSigningSettingsSavedConfigMalformedJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"commit_signing":`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := SigningSettings{Path: path}
	_, err := store.savedConfig()
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("savedConfig(malformed) error = %v, want wrapped ErrSetup", err)
	}
	if !strings.Contains(err.Error(), "invalid runtime configuration") {
		t.Fatalf("savedConfig() error = %v, want invalid runtime configuration", err)
	}
}

func TestSigningSettingsSaveLoadError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := SigningSettings{Path: path}
	err := store.save(gitsigning.Config{Enabled: true})
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("save(load error) error = %v, want wrapped ErrSetup", err)
	}
	if !strings.Contains(err.Error(), "cannot load runtime configuration") {
		t.Fatalf("save() error = %v, want cannot load runtime configuration", err)
	}
}

func TestSigningSettingsSaveWriteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	store := SigningSettings{Path: filepath.Join(dir, "config.json")}
	err := store.save(gitsigning.Config{Enabled: true})
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("save(write error) error = %v, want wrapped ErrSetup", err)
	}
	if !strings.Contains(err.Error(), "cannot save signing settings") {
		t.Fatalf("save() error = %v, want cannot save signing settings", err)
	}
}

func TestSigningSettingsConfigureConfigErrorFromInvalidSettings(t *testing.T) {
	clearSigningEnvironment(t)
	store := SigningSettings{Path: filepath.Join(t.TempDir(), "config.json")}
	if err := os.WriteFile(store.Path, []byte(`{"commit_signing":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := store.Configure(context.Background(), CommitSigningRequest{})
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Configure(invalid settings) error = %v, want wrapped ErrSetup", err)
	}
}

func TestSigningSettingsConfigureSaveAfterImportFails(t *testing.T) {
	clearSigningEnvironment(t)
	dir := t.TempDir()
	// Pre-create the signing key directory (writable in its own right) so
	// gitsigning.Import succeeds; only writing the runtime config JSON into
	// the now-read-only parent directory should fail.
	if err := os.MkdirAll(filepath.Join(dir, "signing"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	store := SigningSettings{Path: filepath.Join(dir, "config.json")}
	raw := signingTestPrivateKey(t)
	_, err := store.Configure(context.Background(), CommitSigningRequest{PrivateKey: raw, Passphrase: "test-passphrase"})
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Configure(import, unwritable dir) error = %v, want wrapped ErrSetup", err)
	}
	if !strings.Contains(err.Error(), "cannot save signing settings") {
		t.Fatalf("Configure() error = %v, want cannot save signing settings", err)
	}
}

func TestSigningSettingsConfigureLoadFailsAfterIdentity(t *testing.T) {
	clearSigningEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.json")
	keyDir := filepath.Join(filepath.Dir(path), "signing")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			// Simulate the on-disk signing key disappearing between import and
			// the identity check that immediately follows it.
			_ = os.RemoveAll(keyDir)
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 123})
			return
		}
		json.NewEncoder(w).Encode([]any{})
	}))
	defer server.Close()

	store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
	raw := signingTestPrivateKey(t)
	state, err := store.Configure(context.Background(), CommitSigningRequest{PrivateKey: raw, Passphrase: "test-passphrase"})
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Configure(key removed before load) error = %v, want wrapped ErrSetup", err)
	}
	if !state.Configured {
		t.Fatalf("state = %+v, want imported key to remain visible", state)
	}
}

func TestSigningSettingsConfigureSnapshotTestFails(t *testing.T) {
	clearSigningEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 123})
			return
		}
		json.NewEncoder(w).Encode([]any{})
	}))
	defer server.Close()

	store := SigningSettings{
		Path:   path,
		Runner: failingRunner{},
		GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} },
	}
	raw := signingTestPrivateKey(t)
	_, err := store.Configure(context.Background(), CommitSigningRequest{PrivateKey: raw, Passphrase: "test-passphrase"})
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Configure(broken git toolchain) error = %v, want wrapped ErrSetup", err)
	}
	if !strings.Contains(err.Error(), "local signing check failed") {
		t.Fatalf("Configure() error = %v, want local signing check failure", err)
	}
}

func TestSigningSettingsConfigureFinalSaveFails(t *testing.T) {
	clearSigningEnvironment(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	login := "test-bot"
	publicKeys := []map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": login, "id": 123})
			return
		}
		json.NewEncoder(w).Encode(publicKeys)
	}))
	defer server.Close()

	store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
	raw := signingTestPrivateKey(t)
	state, err := store.Configure(context.Background(), CommitSigningRequest{PrivateKey: raw, Passphrase: "test-passphrase"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	publicKeys = append(publicKeys, map[string]string{"key": state.PublicKey})

	// Make the directory unwritable only after the key is safely imported and
	// registered, so the next Configure call reaches its final save and fails
	// there instead of failing during import.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err = store.Configure(context.Background(), CommitSigningRequest{Enabled: true})
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Configure(enable, unwritable dir) error = %v, want wrapped ErrSetup", err)
	}
	if !strings.Contains(err.Error(), "cannot save signing settings") {
		t.Fatalf("Configure() error = %v, want cannot save signing settings", err)
	}
}

func TestSigningSettingsConfigureRegisteredCheckUnavailable(t *testing.T) {
	clearSigningEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 123})
			return
		}
		// Malformed JSON makes Registered() return a transport-level error
		// rather than a clean "not registered" result.
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()

	store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
	raw := signingTestPrivateKey(t)
	// Enabled is left false, so the early-return registration guard does not
	// trigger; only the trailing "registration unavailable" bookkeeping does.
	state, err := store.Configure(context.Background(), CommitSigningRequest{PrivateKey: raw, Passphrase: "test-passphrase"})
	if err != nil {
		t.Fatalf("Configure() error = %v, want nil (registration check failure is non-fatal here)", err)
	}
	if state.Registration != "unavailable" {
		t.Fatalf("state.Registration = %q, want unavailable", state.Registration)
	}
}

func TestSigningSettingsVerifyConfigError(t *testing.T) {
	clearSigningEnvironment(t)
	store := SigningSettings{Path: filepath.Join(t.TempDir(), "config.json")}
	if err := os.WriteFile(store.Path, []byte(`{"commit_signing":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := store.Verify(context.Background())
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Verify(invalid settings) error = %v, want wrapped ErrSetup", err)
	}
}

func TestSigningSettingsVerifyCheckError(t *testing.T) {
	clearSigningEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.json")
	store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: "://invalid"} }}
	if err := store.save(gitsigning.Config{Enabled: true, Account: "someone"}); err != nil {
		t.Fatal(err)
	}
	state, err := store.Verify(context.Background())
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Verify(check error) error = %v, want wrapped ErrSetup", err)
	}
	if state.Registration != "not_ready" {
		t.Fatalf("state.Registration = %q, want not_ready", state.Registration)
	}
}

func TestSigningSettingsSnapshotConfigError(t *testing.T) {
	clearSigningEnvironment(t)
	store := SigningSettings{Path: filepath.Join(t.TempDir(), "config.json")}
	if err := os.WriteFile(store.Path, []byte(`{"commit_signing":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := store.Snapshot(context.Background())
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("Snapshot(invalid settings) error = %v, want wrapped ErrSetup", err)
	}
}

func TestSigningSettingsCheckRegisteredError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": int64(123)})
	}))
	defer server.Close()

	store := SigningSettings{GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
	cfg := gitsigning.Config{
		Account:   "test-bot",
		AccountID: 123,
		Email:     "123+test-bot@users.noreply.github.com",
		// An invalid public key makes Registered() fail outright instead of
		// returning a clean "not registered" result.
		PublicKey: "not-a-valid-ssh-key",
	}
	err := store.check(context.Background(), cfg)
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("check(invalid public key) error = %v, want wrapped ErrSetup", err)
	}
}

func TestSigningSettingsCheckLoadError(t *testing.T) {
	t.Parallel()
	// A syntactically valid public key that Registered() will accept, paired
	// with a KeyID whose private key file was never written to keyDir, so
	// gitsigning.Load fails at the "signing key is missing" stage.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": int64(123)})
			return
		}
		json.NewEncoder(w).Encode([]map[string]string{{"key": publicKey}})
	}))
	defer server.Close()

	store := SigningSettings{
		Path:   filepath.Join(t.TempDir(), "config.json"),
		GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} },
	}
	cfg := gitsigning.Config{
		Account:   "test-bot",
		AccountID: 123,
		Email:     "123+test-bot@users.noreply.github.com",
		PublicKey: publicKey,
		KeyID:     strings.Repeat("a", 64),
	}
	checkErr := store.check(context.Background(), cfg)
	if !errors.Is(checkErr, gitsigning.ErrSetup) {
		t.Fatalf("check(missing key file) error = %v, want wrapped ErrSetup", checkErr)
	}
	if !strings.Contains(checkErr.Error(), "signing key is missing") {
		t.Fatalf("check() error = %v, want signing key missing", checkErr)
	}
}
