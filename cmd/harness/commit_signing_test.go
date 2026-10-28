package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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

func TestSigningSetupRequiredOnlyForEnabledMissingKey(t *testing.T) {
	t.Setenv("HARNESS_COMMIT_SIGNING_ENABLED", "true")
	store := signingSettings(localruntime.InitConfig{RuntimeConfigPath: filepath.Join(t.TempDir(), "runtime.json")}, execx.OSRunner{})
	err := store.ApplyEnvironment(context.Background())
	if !signingSetupRequired(context.Background(), store, err) {
		t.Fatalf("missing required key was not recognized as UI-recoverable: %v", err)
	}

	t.Setenv("HARNESS_COMMIT_SIGNING_ENABLED", "invalid")
	err = store.ApplyEnvironment(context.Background())
	if signingSetupRequired(context.Background(), store, err) {
		t.Fatalf("invalid signing policy was treated as UI-recoverable: %v", err)
	}
}

func TestSigningEnvironmentCLIRejectsInvalidPolicyAtStartup(t *testing.T) {
	t.Setenv("HARNESS_COMMIT_SIGNING_ENABLED", "invalid")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", "")
	t.Setenv("HARNESS_COMMIT_SIGNING_PASSPHRASE", "")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", filepath.Join(t.TempDir(), "runtime.json"))
	result := runSingleHarness(context.Background(), config.Config{}, func(string, ...any) {})
	if result.ExitCode != app.ExitConfig || !errors.Is(result.Err, gitsigning.ErrSetup) {
		t.Fatalf("run accepted invalid signing policy: %+v", result)
	}
	path := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(path, []byte(`{"repoUrl":"https://github.com/example/repo","prompt":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runMultiplex([]string{"--config", path}); code != app.ExitConfig {
		t.Fatalf("multiplex exit = %d, want %d", code, app.ExitConfig)
	}
	if code := runServer(nil); code != app.ExitConfig {
		t.Fatalf("serve exit = %d, want %d", code, app.ExitConfig)
	}
}
