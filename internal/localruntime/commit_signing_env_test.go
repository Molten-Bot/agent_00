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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"golang.org/x/crypto/ssh"
)

func clearSigningEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{commitSigningEnabledEnv, "HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", "HARNESS_COMMIT_SIGNING_PASSPHRASE"} {
		t.Setenv(name, "")
	}
}

// signingTestKeyPair returns the shared test private key and its public half.
func signingTestKeyPair(t *testing.T) (raw string, public string) {
	t.Helper()
	raw = signingTestPrivateKey(t)
	signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(raw), []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	return raw, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// signingTestGitHub answers the identity and signing-key lookups gitsigning
// makes. The returned flag toggles whether the account lists public as a
// signing key, which is how a previously good key stops validating.
func signingTestGitHub(t *testing.T, public string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
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
	t.Cleanup(server.Close)
	return server, &registered
}

func TestSigningEnvironmentStartup(t *testing.T) {
	clearSigningEnvironment(t)
	raw, public := signingTestKeyPair(t)
	server, registered := signingTestGitHub(t, public)
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

func TestSigningSetupRequirementHoldsTasksUntilKeyExists(t *testing.T) {
	clearSigningEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.json")
	store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: "://invalid"} }}

	if err := store.SetupRequirement(); err != nil {
		t.Fatalf("signing off should not hold tasks: %v", err)
	}

	// The reported failure: required by environment, no key saved anywhere.
	t.Setenv(commitSigningEnabledEnv, "true")
	err := store.SetupRequirement()
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("expected a setup hold: %v", err)
	}
	if !strings.Contains(err.Error(), commitSigningEnabledEnv) {
		t.Fatalf("hold should name the environment variable: %v", err)
	}
	state := store.ApplySetupRequirement(CommitSigningState{})
	if !state.SetupRequired || !state.EnvEnforced || state.Message != err.Error() {
		t.Fatalf("ui state should carry the hold: %+v", state)
	}

	// A saved enabled flag without a key holds tasks too, without blaming the
	// environment it did not come from.
	t.Setenv(commitSigningEnabledEnv, "")
	if err := store.save(gitsigning.Config{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	err = store.SetupRequirement()
	if !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("expected a setup hold for the saved preference: %v", err)
	}
	if strings.Contains(err.Error(), commitSigningEnabledEnv) {
		t.Fatalf("saved preference should not blame the environment: %v", err)
	}

	// A key that imports but cannot be validated keeps the hold: it is saved,
	// yet nothing has shown it can sign a task commit for this account.
	if _, err := store.Configure(context.Background(), CommitSigningRequest{PrivateKey: signingTestPrivateKey(t), Passphrase: "test-passphrase"}); !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("offline identity lookup should still import the key: %v", err)
	}
	t.Setenv(commitSigningEnabledEnv, "true")
	held := store.SetupRequirement()
	if !errors.Is(held, ErrSigningSetupRequired) {
		t.Fatalf("an unvalidated key released the hold: %v", held)
	}
	if state := store.ApplySetupRequirement(CommitSigningState{}); !state.SetupRequired || state.Message != held.Error() {
		t.Fatalf("ui state should explain the hold: %+v", state)
	}
}

// A key saved by an earlier boot can stop working: revoked on GitHub, or a
// local signing toolchain that broke. ApplyEnvironment keeps the UI reachable
// so the operator can fix it, so the hold has to outlive that call. Otherwise
// SetupRequirement sees a saved key, reports nothing, and serve admits tasks
// signed by a key this boot already rejected.
func TestSigningBootFailureHoldsTasksUntilSettingsValidate(t *testing.T) {
	clearSigningEnvironment(t)
	raw, public := signingTestKeyPair(t)
	server, registered := signingTestGitHub(t, public)
	path := filepath.Join(t.TempDir(), "config.json")
	store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
	ctx := context.Background()

	t.Setenv(commitSigningEnabledEnv, "true")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", raw)
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "test-passphrase")
	if err := store.ApplyEnvironment(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetupRequirement(); err != nil {
		t.Fatalf("a key that validated at boot should not hold tasks: %v", err)
	}

	// Reboot on the saved key alone, after GitHub stopped listing it.
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")
	registered.Store(false)
	bootErr := store.ApplyEnvironment(ctx)
	if !errors.Is(bootErr, ErrSigningSetupRequired) {
		t.Fatalf("serve should hold the UI open instead of exiting: %v", bootErr)
	}
	held := store.SetupRequirement()
	if !errors.Is(held, ErrSigningSetupRequired) {
		t.Fatalf("a saved key that failed boot must keep holding tasks: %v", held)
	}
	state := store.ApplySetupRequirement(CommitSigningState{})
	if !state.SetupRequired || !state.EnvEnforced || state.Message != held.Error() {
		t.Fatalf("ui state should explain the hold: %+v", state)
	}

	// Registering the key on GitHub and checking it releases the hold without a
	// restart. Signing is already enabled here, so the operator has nothing new
	// to save and Check registration is the only way out.
	registered.Store(true)
	if _, err := store.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetupRequirement(); err != nil {
		t.Fatalf("a verified key should release the hold: %v", err)
	}

	// Saving settings clears the hold too, which is the path taken when the
	// operator replaces the key instead of registering the old one.
	registered.Store(false)
	if err := store.ApplyEnvironment(ctx); !errors.Is(err, ErrSigningSetupRequired) {
		t.Fatalf("a second failed boot should hold again: %v", err)
	}
	registered.Store(true)
	if _, err := store.Configure(ctx, CommitSigningRequest{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetupRequirement(); err != nil {
		t.Fatalf("revalidated settings should release the hold: %v", err)
	}
}

// A boot hold can record a failure that has nothing to do with the key, such
// as GitHub being unreachable. Holding submissions is right while it lasts, but
// the hold must not outlive it: the next submission rechecks and clears it
// without the operator pressing Check registration.
func TestSigningSubmissionRequirementRechecksTransientHold(t *testing.T) {
	clearSigningEnvironment(t)
	raw, public := signingTestKeyPair(t)
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 123})
			return
		}
		json.NewEncoder(w).Encode([]map[string]string{{"key": public}})
	}))
	t.Cleanup(server.Close)

	path := filepath.Join(t.TempDir(), "config.json")
	// The recheck is rate limited, so drive its clock instead of waiting out the
	// interval in real time.
	var clock sync.Mutex
	now := time.Now()
	store := SigningSettings{
		Path:   path,
		GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} },
		Now: func() time.Time {
			clock.Lock()
			defer clock.Unlock()
			return now
		},
	}
	advance := func(d time.Duration) {
		clock.Lock()
		defer clock.Unlock()
		now = now.Add(d)
	}
	ctx := context.Background()
	t.Setenv(commitSigningEnabledEnv, "true")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", raw)
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "test-passphrase")
	if err := store.ApplyEnvironment(ctx); err != nil {
		t.Fatal(err)
	}

	// Reboot on the saved key while GitHub is down. The key is fine; the lookup
	// is not, so the boot still has to hold submissions.
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")
	unavailable.Store(true)
	if err := store.ApplyEnvironment(ctx); !errors.Is(err, ErrSigningSetupRequired) {
		t.Fatalf("an unreachable GitHub should hold the UI open: %v", err)
	}
	if err := store.SubmissionRequirement(ctx); !errors.Is(err, ErrSigningSetupRequired) {
		t.Fatalf("submission accepted while signing could not be checked: %v", err)
	}

	unavailable.Store(false)
	// Still inside the interval, so this submission reports the recorded hold
	// rather than paying for a second round of GitHub checks.
	if err := store.SubmissionRequirement(ctx); !errors.Is(err, ErrSigningSetupRequired) {
		t.Fatalf("recheck ran again inside the interval: %v", err)
	}
	advance(signingRecheckInterval)
	if err := store.SubmissionRequirement(ctx); err != nil {
		t.Fatalf("hold outlived the failure that caused it: %v", err)
	}
	if err := store.SetupRequirement(); err != nil {
		t.Fatalf("recheck did not release the hold: %v", err)
	}
}

// ApplyEnvironment revalidates the saved key, which must not clear the existing
// hold up front: a submission that lands while the checks are still running
// would find nothing holding it back.
func TestSigningBootRevalidationKeepsHoldUntilItPasses(t *testing.T) {
	clearSigningEnvironment(t)
	raw, public := signingTestKeyPair(t)
	var registered, blocking, released atomic.Bool
	registered.Store(true)
	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	// A failed assertion must not leave the handler parked, or closing the
	// server would wait on it forever. Cleanups run last-registered-first, so
	// this one unblocks the handler before server.Close runs.
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocking.CompareAndSwap(true, false) {
			reached <- struct{}{}
			<-release
		}
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 123})
			return
		}
		if registered.Load() {
			json.NewEncoder(w).Encode([]map[string]string{{"key": public}})
			return
		}
		json.NewEncoder(w).Encode([]any{})
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)

	path := filepath.Join(t.TempDir(), "config.json")
	store := SigningSettings{Path: path, GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: server.URL, Client: server.Client()} }}
	ctx := context.Background()
	t.Setenv(commitSigningEnabledEnv, "true")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", raw)
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "test-passphrase")
	if err := store.ApplyEnvironment(ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")
	registered.Store(false)
	if err := store.ApplyEnvironment(ctx); !errors.Is(err, ErrSigningSetupRequired) {
		t.Fatalf("an unregistered key should hold the UI open: %v", err)
	}

	registered.Store(true)
	blocking.Store(true)
	done := make(chan error, 1)
	go func() { done <- store.ApplyEnvironment(ctx) }()
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("boot revalidation never reached GitHub")
	}
	if err := store.SetupRequirement(); !errors.Is(err, ErrSigningSetupRequired) {
		t.Errorf("hold was cleared before the key revalidated: %v", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := store.SetupRequirement(); err != nil {
		t.Fatalf("a revalidated key should release the hold: %v", err)
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
