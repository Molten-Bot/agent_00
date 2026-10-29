package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestSigningSettingsCLIUsesRuntimePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", path)
	if err := os.WriteFile(path, []byte(`{"github_token":"test-token","commit_signing":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	store := signingSettings(localruntime.InitConfig{}, execx.OSRunner{})
	if store.Path != path || store.GitHub().Token != "test-token" {
		t.Fatal("CLI did not resolve saved signing configuration and GitHub auth")
	}
	if snapshot, err := store.Snapshot(context.Background()); err != nil || snapshot != nil {
		t.Fatalf("disabled snapshot: %v", err)
	}
	// A malformed store must stop execution before any agent or git operations.
	if err := os.WriteFile(path, []byte(`{"commit_signing":`), 0600); err != nil {
		t.Fatal(err)
	}
	result := runSingleHarness(context.Background(), config.Config{}, func(string, ...any) {})
	if result.ExitCode != app.ExitConfig || !errors.Is(result.Err, gitsigning.ErrSetup) {
		t.Fatalf("CLI did not fail closed: %+v", result)
	}
}

func TestSigningFailuresDoNotQueueCodingWork(t *testing.T) {
	t.Parallel()
	result := app.Result{ExitCode: app.ExitGit, Err: fmt.Errorf("push: %w", gitsigning.ErrSetup)}
	if queue, _ := shouldQueueFailureFollowUp(localSubmitSource, result); queue {
		t.Fatal("signing failure queued coding follow-up")
	}
	if queue, _ := shouldQueueFailureRerun(localSubmitSource, result); queue {
		t.Fatal("signing failure queued rerun")
	}
}

func TestSigningEnvironmentCLIRejectsInvalidPrivateKeyAtStartup(t *testing.T) {
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "not-a-private-key")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", filepath.Join(t.TempDir(), "runtime.json"))
	result := runSingleHarness(context.Background(), config.Config{}, func(string, ...any) {})
	if result.ExitCode != app.ExitConfig || !errors.Is(result.Err, gitsigning.ErrSetup) {
		t.Fatalf("run accepted invalid signing key: %+v", result)
	}
	path := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(path, []byte(`{"repoUrl":"https://github.com/example/repo","prompt":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runMultiplex([]string{"--config", path}); code != app.ExitConfig {
		t.Fatalf("multiplex exit = %d, want %d", code, app.ExitConfig)
	}
	// serve must reject the key before it binds a listener and waits for a
	// shutdown signal. A fall-through would block forever, so bound the wait:
	// otherwise the whole package only fails when go test hits its deadline.
	// Point serve at a port this test already holds so a regression fails to
	// bind and returns instead of leaving a live server behind after t.Fatal.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	serve := make(chan int, 1)
	go func() { serve <- runServer([]string{"--ui-listen", occupied.Addr().String()}) }()
	select {
	case code := <-serve:
		if code != app.ExitConfig {
			t.Fatalf("serve exit = %d, want %d", code, app.ExitConfig)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not reject the invalid signing key and kept running")
	}
}

func TestSigningSubmissionGateHoldsTasksUntilKeyIsSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", path)
	if err := os.WriteFile(path, []byte(`{"commit_signing":{"enabled":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err := signingSubmissionGate(ctx, localruntime.InitConfig{}, execx.OSRunner{})
	if err == nil || !errors.Is(err, gitsigning.ErrSetup) || !errors.Is(err, localruntime.ErrSigningSetupRequired) {
		t.Fatalf("submission accepted while signing was unconfigured: %v", err)
	}

	// Saving a key releases queued work without restarting the server.
	if err := os.WriteFile(path, []byte(`{"commit_signing":{"enabled":true,"key_id":"abc123"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := signingSubmissionGate(ctx, localruntime.InitConfig{}, execx.OSRunner{}); err != nil {
		t.Fatalf("submission still blocked after saving a key: %v", err)
	}

	// Signing off leaves submissions unblocked.
	if err := os.WriteFile(path, []byte(`{"commit_signing":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := signingSubmissionGate(ctx, localruntime.InitConfig{}, execx.OSRunner{}); err != nil {
		t.Fatalf("submission blocked with signing disabled: %v", err)
	}
}

func TestSavedSigningWithoutKeyHoldsSetupInsteadOfFailingStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", path)
	if err := os.WriteFile(path, []byte(`{"commit_signing":{"enabled":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")

	err := signingSettings(localruntime.InitConfig{}, execx.OSRunner{}).ApplyEnvironment(context.Background())
	if err == nil || !errors.Is(err, localruntime.ErrSigningSetupRequired) {
		t.Fatalf("required signing without a key = %v, want a setup hold serve can boot through", err)
	}
}

func TestSigningEnvironmentRejectsUnusableProvidedKeyAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", path)
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "not-a-private-key")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")
	// A previously saved key must not quietly take over for the supplied one.
	if err := os.WriteFile(path, []byte(`{"commit_signing":{"enabled":true,"key_id":"abc123"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	err := signingSettings(localruntime.InitConfig{}, execx.OSRunner{}).ApplyEnvironment(context.Background())
	if err == nil || !errors.Is(err, gitsigning.ErrSetup) {
		t.Fatalf("unusable environment key = %v, want a signing setup error", err)
	}
	if errors.Is(err, localruntime.ErrSigningSetupRequired) {
		t.Fatalf("unusable environment key was held instead of stopping startup: %v", err)
	}
}
