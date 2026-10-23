package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"golang.org/x/crypto/ssh"
)

func clearSigningEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{commitSigningEnabledEnv, "HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", "HARNESS_COMMIT_SIGNING_PASSPHRASE"} {
		t.Setenv(name, "")
	}
}

func TestSigningEnvironmentStartup(t *testing.T) {
	clearSigningEnvironment(t)
	raw := signingTestPrivateKey(t)
	signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(raw), []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	var registered atomic.Bool
	registered.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 123})
		} else if registered.Load() {
			json.NewEncoder(w).Encode([]map[string]string{{"key": public}})
		} else {
			json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer server.Close()
	for _, source := range []string{"inline", "file"} {
		t.Run(source, func(t *testing.T) {
			clearSigningEnvironment(t)
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(`{"github_token":"test-token","commit_signing":{"enabled":false}}`), 0600); err != nil {
				t.Fatal(err)
			}
			store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
			t.Setenv(commitSigningEnabledEnv, "true")
			t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "test-passphrase")
			if source == "inline" {
				t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", raw)
			} else {
				keyPath := filepath.Join(t.TempDir(), "key")
				if err := os.WriteFile(keyPath, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", keyPath)
			}
			ctx := context.Background()
			if err := store.ApplyEnvironment(ctx); err != nil {
				t.Fatal(err)
			}
			state, err := store.Status(ctx)
			if err != nil || !state.Enabled || state.PublicKey != public {
				t.Fatalf("startup state: %+v %v", state, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "PRIVATE KEY") || strings.Contains(string(data), "test-passphrase") {
				t.Fatal("secret persisted in JSON")
			}
			if ReadRuntimeConfigString(path, "github_token") != "test-token" {
				t.Fatal("unrelated config lost")
			}
			if _, err := store.Configure(ctx, CommitSigningRequest{Enabled: false}); !errors.Is(err, gitsigning.ErrSetup) {
				t.Fatalf("UI disabled environment policy: %v", err)
			}
			// Reboot with only the enabled flag reuses the managed key.
			t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "")
			t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", "")
			t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")
			if err := store.ApplyEnvironment(ctx); err != nil {
				t.Fatal(err)
			}
			// A saved disabled flag cannot defeat the environment requirement.
			cfg, err := store.savedConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Enabled = false
			if err := store.save(cfg); err != nil {
				t.Fatal(err)
			}
			if snapshot, err := store.Snapshot(ctx); err != nil || snapshot == nil {
				t.Fatalf("disabled saved flag overrode env: %v", err)
			}
			registered.Store(false)
			if err := store.ApplyEnvironment(ctx); !errors.Is(err, gitsigning.ErrSetup) {
				t.Fatalf("unregistered key accepted at boot: %v", err)
			}
			if _, err := store.Snapshot(ctx); !errors.Is(err, gitsigning.ErrSetup) {
				t.Fatalf("failed startup fell back to unsigned: %v", err)
			}
			registered.Store(true)
			t.Setenv(commitSigningEnabledEnv, "false")
			if err := store.ApplyEnvironment(ctx); err != nil {
				t.Fatal(err)
			}
			if snapshot, err := store.Snapshot(ctx); err != nil || snapshot != nil {
				t.Fatalf("explicit false ignored: %v", err)
			}
		})
	}
}

func TestSigningEnvironmentInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"invalid bool", map[string]string{commitSigningEnabledEnv: "secret-invalid-bool"}},
		{"invalid key", map[string]string{"HARNESS_COMMIT_SIGNING_PRIVATE_KEY": "secret-invalid-key"}},
		{"conflicting sources", map[string]string{"HARNESS_COMMIT_SIGNING_PRIVATE_KEY": "secret-key", "HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE": "secret-path"}},
		{"missing file", map[string]string{"HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE": "/missing/secret-path"}},
		{"passphrase without key", map[string]string{"HARNESS_COMMIT_SIGNING_PASSPHRASE": "secret-passphrase"}},
		{"enabled without key", map[string]string{commitSigningEnabledEnv: "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSigningEnvironment(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			store := SigningSettings{Path: filepath.Join(t.TempDir(), "config.json"), GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: "://invalid"} }}
			err := store.ApplyEnvironment(context.Background())
			if !errors.Is(err, gitsigning.ErrSetup) {
				t.Fatalf("expected setup error: %v", err)
			}
			if strings.Contains(err.Error(), "secret-") {
				t.Fatalf("input leaked: %v", err)
			}
		})
	}
}

func TestSigningEnvironmentUnsetPreservesSettings(t *testing.T) {
	clearSigningEnvironment(t)
	store := SigningSettings{Path: filepath.Join(t.TempDir(), "config.json")}
	ctx := context.Background()
	if err := store.ApplyEnvironment(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
		t.Fatal("unset environment wrote config")
	}
	for _, enabled := range []bool{false, true} {
		if err := store.save(gitsigning.Config{Enabled: enabled}); err != nil {
			t.Fatal(err)
		}
		if err := store.ApplyEnvironment(ctx); err != nil {
			t.Fatal(err)
		}
		state, err := store.Status(ctx)
		if err != nil || state.Enabled != enabled {
			t.Fatalf("saved preference changed: %+v %v", state, err)
		}
	}
}
