package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func selectableGitHubReadyRunner() *authGateRunnerStub {
	return &authGateRunnerStub{
		run: func(_ context.Context, cmd execx.Command) (execx.Result, error) {
			if got, want := cmd.Name, "gh"; got != want {
				return execx.Result{}, nil
			}
			if got, want := strings.Join(cmd.Args, " "), gitHubTokenValidationArgsStringForTest(); got != want {
				return execx.Result{}, nil
			}
			return execx.Result{Stdout: "github.com logged in"}, nil
		},
	}
}

func TestSelectableAgentAuthGateStatusRequiresGitHubTokenWhenUnbound(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	gate := newSelectableAgentAuthGate(context.Background(), nil, localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got, want := state.State, "needs_configure"; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
	if got, want := state.ConfigureCommand, claudeGitHubConfigureCommand; got != want {
		t.Fatalf("ConfigureCommand = %q, want %q", got, want)
	}
}

func TestSelectableAgentAuthGateStartVerifyAndErrorState(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	gate := newSelectableAgentAuthGate(context.Background(), nil, localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	startState, err := gate.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}
	if got, want := startState.State, "needs_configure"; got != want {
		t.Fatalf("StartDeviceAuth() state = %q, want %q", got, want)
	}
	verifyState, err := gate.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got, want := verifyState.State, "needs_configure"; got != want {
		t.Fatalf("Verify() state = %q, want %q", got, want)
	}
	errorState := gate.errorState(" ")
	if got, want := errorState.Message, "agent auth status failed"; got != want {
		t.Fatalf("errorState(empty).Message = %q, want %q", got, want)
	}
}

func TestSelectableAgentAuthGateStatusRequiresHarnessSelectionAfterGitHubReady(t *testing.T) {
	t.Setenv("GH_TOKEN", "github_token_ready_token")
	t.Setenv("GITHUB_TOKEN", "")

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	gate := newSelectableAgentAuthGate(context.Background(), selectableGitHubReadyRunner(), localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got, want := state.State, agentHarnessSelectionState; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
	if len(state.ConfigureOptions) == 0 {
		t.Fatal("ConfigureOptions = empty, want harness options")
	}
	gotOrder := make([]string, 0, len(state.ConfigureOptions))
	for _, option := range state.ConfigureOptions {
		gotOrder = append(gotOrder, option.Value)
	}
	wantOrder := []string{"claude", "codex"}
	if len(gotOrder) < len(wantOrder) {
		t.Fatalf("ConfigureOptions count = %d, want at least %d", len(gotOrder), len(wantOrder))
	}
	for i, want := range wantOrder {
		if gotOrder[i] != want {
			t.Fatalf("harness option[%d] = %q, want %q (full=%v)", i, gotOrder[i], want, gotOrder)
		}
	}
}

func TestSelectableAgentAuthGateStatusRequiresHarnessSelectionAfterValidatedGITHUBTOKEN(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "github_token_ready_token")

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	runner := &authGateRunnerStub{
		run: func(_ context.Context, cmd execx.Command) (execx.Result, error) {
			if got, want := cmd.Name, "gh"; got != want {
				t.Fatalf("command = %q, want %q", got, want)
			}
			if got, want := strings.Join(cmd.Args, " "), gitHubTokenValidationArgsStringForTest(); got != want {
				t.Fatalf("args = %q, want %q", got, want)
			}
			if got, want := os.Getenv("GITHUB_TOKEN"), "github_token_ready_token"; got != want {
				t.Fatalf("GITHUB_TOKEN = %q, want %q during validation", got, want)
			}
			return execx.Result{Stdout: "github.com logged in"}, nil
		},
	}
	gate := newSelectableAgentAuthGate(context.Background(), runner, localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got, want := state.State, agentHarnessSelectionState; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
	if got := state.ConfigureCommand; got != "" {
		t.Fatalf("ConfigureCommand = %q, want empty once GitHub token is ready", got)
	}
}

func TestSelectableAgentAuthGateStatusAcceptsMalformedDockerComposeGitHubEnv(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN:github_token_ready_token", "")

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	runner := &authGateRunnerStub{
		run: func(_ context.Context, _ execx.Command) (execx.Result, error) {
			if got, want := os.Getenv("GITHUB_TOKEN"), "github_token_ready_token"; got != want {
				t.Fatalf("GITHUB_TOKEN = %q, want %q during validation", got, want)
			}
			return execx.Result{Stdout: "github.com logged in"}, nil
		},
	}
	gate := newSelectableAgentAuthGate(context.Background(), runner, localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got, want := state.State, agentHarnessSelectionState; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
	if got := state.ConfigureCommand; got != "" {
		t.Fatalf("ConfigureCommand = %q, want empty once GitHub token is ready", got)
	}
}

func TestSelectableAgentAuthGateConfigurePersistsSelectionAndRejectsSwitch(t *testing.T) {
	t.Setenv("GH_TOKEN", "github_token_ready_token")
	t.Setenv("GITHUB_TOKEN", "")

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	gate := newSelectableAgentAuthGate(context.Background(), selectableGitHubReadyRunner(), localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Configure(context.Background(), "claude")
	if err != nil {
		t.Fatalf("Configure(claude) error = %v", err)
	}
	if got, want := state.Harness, "claude"; got != want {
		t.Fatalf("Harness = %q, want %q", got, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(config.json) error = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal(config.json) error = %v", err)
	}
	if got, want := doc["agent_harness"], "claude"; got != want {
		t.Fatalf("agent_harness = %#v, want %q", got, want)
	}
	if got, want := doc["agent_command"], "claude"; got != want {
		t.Fatalf("agent_command = %#v, want %q", got, want)
	}

	_, err = gate.Configure(context.Background(), "codex")
	if err == nil {
		t.Fatal("Configure(codex) error = nil, want non-nil")
	}
	if got := strings.ToLower(err.Error()); !strings.Contains(got, "already configured") || !strings.Contains(got, "cannot be changed") {
		t.Fatalf("Configure(codex) error = %q, want lock-once guidance", err)
	}

	// Re-selecting the harness that is already bound should simply return the
	// current status rather than treating it as a rejected switch.
	reselectState, err := gate.Configure(context.Background(), "claude")
	if err != nil {
		t.Fatalf("Configure(claude) re-select error = %v", err)
	}
	if got, want := reselectState.Harness, "claude"; got != want {
		t.Fatalf("re-select Harness = %q, want %q", got, want)
	}
}

func blockedRuntimeConfigPath(t *testing.T) string {
	t.Helper()
	blockerFile := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocker) error = %v", err)
	}
	return filepath.Join(blockerFile, "config.json")
}

func TestSelectableAgentAuthGateStatusReturnsErrorWhenRuntimeConfigUnreadable(t *testing.T) {
	path := blockedRuntimeConfigPath(t)
	gate := newSelectableAgentAuthGate(context.Background(), nil, localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Status(context.Background())
	if err == nil {
		t.Fatal("Status() error = nil, want load runtime config failure")
	}
	if state.State != "error" {
		t.Fatalf("state = %+v, want error state", state)
	}
	if !strings.Contains(state.Message, "load runtime config") {
		t.Fatalf("state.Message = %q, want load runtime config detail", state.Message)
	}
}

func TestSelectableAgentAuthGateConfigureReturnsErrorWhenRuntimeConfigUnreadable(t *testing.T) {
	path := blockedRuntimeConfigPath(t)
	gate := newSelectableAgentAuthGate(context.Background(), nil, localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Configure(context.Background(), "claude")
	if err == nil {
		t.Fatal("Configure() error = nil, want load runtime config failure")
	}
	if state.State != "error" {
		t.Fatalf("state = %+v, want error state", state)
	}
	if !strings.Contains(state.Message, "load runtime config") {
		t.Fatalf("state.Message = %q, want load runtime config detail", state.Message)
	}
}

func TestSelectableAgentAuthGateConfigureAppliesGitHubTokenWhenNeedsConfigure(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	useSuccessfulGitHubStarTestServer(t)

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	gate := newSelectableAgentAuthGate(context.Background(), selectableGitHubReadyRunner(), localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Configure(context.Background(), fakeGitHubPAT("selectable_needs_configure"))
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if state.State != agentHarnessSelectionState {
		t.Fatalf("state.State = %q, want harness selection state once GitHub token is applied", state.State)
	}
}

func TestSelectableAgentAuthGateConfigureRejectsUnsupportedHarnessSelection(t *testing.T) {
	t.Setenv("GH_TOKEN", "github_token_ready_token")
	t.Setenv("GITHUB_TOKEN", "")

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	gate := newSelectableAgentAuthGate(context.Background(), selectableGitHubReadyRunner(), localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Configure(context.Background(), "not-a-real-harness")
	if err == nil || err.Error() != "agent selection is required" {
		t.Fatalf("Configure(unsupported) error = %v, want agent selection required", err)
	}
	if state.State != agentHarnessSelectionState {
		t.Fatalf("state.State = %q, want %q", state.State, agentHarnessSelectionState)
	}
	if got, want := state.Message, "Select an agent to continue."; got != want {
		t.Fatalf("state.Message = %q, want %q", got, want)
	}
}

func TestSelectableAgentAuthGateConfigureSaveFailure(t *testing.T) {
	t.Setenv("GH_TOKEN", "github_token_ready_token")
	t.Setenv("GITHUB_TOKEN", "")

	readOnlyDir := t.TempDir()
	if err := os.Chmod(readOnlyDir, 0o555); err != nil {
		t.Fatalf("Chmod(read-only) error = %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(readOnlyDir, 0o755)
	})

	path := filepath.Join(readOnlyDir, "config.json")
	gate := newSelectableAgentAuthGate(context.Background(), selectableGitHubReadyRunner(), localruntime.InitConfig{RuntimeConfigPath: path}, nil)

	state, err := gate.Configure(context.Background(), "claude")
	if err == nil {
		t.Fatal("Configure() error = nil, want save failure")
	}
	if state.State != agentHarnessSelectionState {
		t.Fatalf("state = %+v, want harness selection state with save-failure message", state)
	}
	if !strings.Contains(state.Message, "Save selected agent failed") {
		t.Fatalf("state.Message = %q, want save-failure detail", state.Message)
	}
}
