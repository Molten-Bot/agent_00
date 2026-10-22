package localruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"golang.org/x/crypto/ssh"
)

func signingTestPrivateKey(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, "test", []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

func TestSigningSettingsLifecycle(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	registered := []map[string]string{}
	login := "test-bot"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": login, "id": 123, "name": "Test Bot"})
			return
		}
		json.NewEncoder(w).Encode(registered)
	}))
	defer server.Close()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.json")
	store := SigningSettings{Path: path, Runner: execx.OSRunner{}, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
	if snapshot, err := store.Snapshot(ctx); err != nil || snapshot != nil {
		t.Fatalf("default not disabled: %v", err)
	}
	raw := signingTestPrivateKey(t)
	state, err := store.Configure(ctx, CommitSigningRequest{PrivateKey: raw, Passphrase: "test-passphrase"})
	if err != nil || !state.Configured || state.Enabled || state.Registered {
		t.Fatalf("import: %+v %v", state, err)
	}
	initialPublic := state.PublicKey
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE KEY") || strings.Contains(string(data), "test-passphrase") {
		t.Fatal("secret in config")
	}
	if _, err := store.Configure(ctx, CommitSigningRequest{Enabled: true}); !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("enabled unregistered: %v", err)
	}
	mu.Lock()
	registered = append(registered, map[string]string{"key": state.PublicKey})
	mu.Unlock()
	if state, err = store.Verify(ctx); err != nil || !state.Registered {
		t.Fatalf("verify: %+v %v", state, err)
	}
	if state, err = store.Configure(ctx, CommitSigningRequest{Enabled: true}); err != nil || !state.Enabled {
		t.Fatalf("enable: %+v %v", state, err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the store and preserve unrelated runtime settings across saves.
	restarted := store
	restarted.Initial = InitConfig{}
	if err := SaveRuntimeConfigGitHubToken(path, InitConfig{}, "token-test-only"); err != nil {
		t.Fatal(err)
	}
	if state, err = restarted.Status(ctx); err != nil || !state.Enabled {
		t.Fatalf("restart: %+v %v", state, err)
	}
	if _, err = store.Configure(ctx, CommitSigningRequest{Enabled: true, PrivateKey: "bad-secret"}); !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("invalid replacement: %v", err)
	}
	state, _ = store.Status(ctx)
	if state.PublicKey != initialPublic || !state.Enabled {
		t.Fatal("failed replacement changed settings")
	}
	// Import a second key while disabled. The old immutable snapshot still signs.
	newKey := signingTestPrivateKey(t)
	state, err = store.Configure(ctx, CommitSigningRequest{PrivateKey: newKey, Passphrase: "test-passphrase"})
	if err != nil || state.PublicKey == initialPublic || state.Enabled {
		t.Fatalf("replacement: %+v %v", state, err)
	}
	if err := snapshot.Test(ctx, execx.OSRunner{}); err != nil {
		t.Fatalf("old running snapshot broken: %v", err)
	}
	if value := ReadRuntimeConfigString(path, "github_token"); value != "token-test-only" {
		t.Fatal("unrelated setting lost")
	}
	mu.Lock()
	registered = append(registered, map[string]string{"key": state.PublicKey})
	mu.Unlock()
	if _, err = store.Configure(ctx, CommitSigningRequest{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	login = "different-user"
	mu.Unlock()
	if _, err = store.Snapshot(ctx); !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("account change accepted: %v", err)
	}
	if _, err = store.Configure(ctx, CommitSigningRequest{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Status(ctx)
	if state.Enabled || !state.Configured {
		t.Fatal("disable did not retain key")
	}
}

func TestSigningSettingsConcurrentUpdates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	store := SigningSettings{Path: path}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Configure(context.Background(), CommitSigningRequest{}); err != nil {
				t.Error(err)
			}
			if err := SaveRuntimeConfigGitHubToken(path, InitConfig{}, "token-test-only"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := store.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ReadRuntimeConfigString(path, "github_token") != "token-test-only" {
		t.Fatal("concurrent update lost")
	}
}

func TestSigningSettingsMissingKeyFailsClosed(t *testing.T) {
	t.Parallel()
	store := SigningSettings{Path: filepath.Join(t.TempDir(), "config.json")}
	if err := store.save(gitsigning.Config{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Avoid a real API request: an unavailable local transport is a setup error too.
	store.GitHub = func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: "://invalid"} }
	if _, err := store.Snapshot(context.Background()); !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("enabled missing config accepted: %v", err)
	}
}
