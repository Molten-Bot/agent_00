package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// withUnreadableCodexAuthMarker points CODEX_HOME at a directory containing an
// auth-failure marker file this process cannot read, forcing
// agentruntime.CheckCodexAuthentication to return a generic (non
// ErrCodexAuthRequired) error so the gate's error-propagation branches can be
// exercised deterministically.
func withUnreadableCodexAuthMarker(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	markerPath := filepath.Join(home, ".agent00-auth-required")
	if err := os.WriteFile(markerPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(marker) error = %v", err)
	}
	if err := os.Chmod(markerPath, 0o000); err != nil {
		t.Fatalf("Chmod(marker) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(markerPath, 0o600) })
	t.Setenv("CODEX_HOME", home)
}

func TestCodexAuthGatePropagatesGenericAuthCheckErrors(t *testing.T) {
	withUnreadableCodexAuthMarker(t)

	g := &codexAuthGate{
		runner:    &authGateRunnerStub{},
		command:   "codex",
		required:  true,
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
	}

	if err := g.updateAuthenticationFailure(); err == nil || errors.Is(err, agentruntime.ErrCodexAuthRequired) {
		t.Fatalf("updateAuthenticationFailure() error = %v, want generic non-auth-required error", err)
	}

	if _, err := g.Status(context.Background()); err == nil {
		t.Fatal("Status() error = nil, want generic error from auth check")
	}

	if _, err := g.StartDeviceAuth(context.Background()); err == nil {
		t.Fatal("StartDeviceAuth() error = nil, want generic error from auth check")
	}

	ready, msg, err := g.probe(context.Background())
	if err == nil || ready || msg != "" {
		t.Fatalf("probe() = (%v, %q, %v), want generic error", ready, msg, err)
	}
}

func TestProbeClassifiesAPIKeyMentionWithoutLoggedInPhrase(t *testing.T) {
	t.Parallel()

	runner := &authGateRunnerStub{run: func(_ context.Context, _ execx.Command) (execx.Result, error) {
		return execx.Result{}, errors.New("valid api key detected, please retry")
	}}
	g := &codexAuthGate{runner: runner, command: "codex"}

	ready, msg, err := g.probe(context.Background())
	if err != nil || !ready {
		t.Fatalf("probe() = (%v, %q, %v), want ready classification from api key mention", ready, msg, err)
	}
	if got, want := msg, "Codex authorization is ready."; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestCodexConfigureNilReceiverReturnsReady(t *testing.T) {
	t.Parallel()

	var g *codexAuthGate
	status, err := g.Configure(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("Configure(nil gate) error = %v", err)
	}
	if !status.Ready || status.Required || status.State != "ready" {
		t.Fatalf("Configure(nil gate) = %+v", status)
	}
}

func TestVerifyBlockedByMissingGitHubToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	g := &codexAuthGate{
		runner:             &authGateRunnerStub{},
		command:            "codex",
		requireGitHubToken: true,
		runtimeConfigPath:  filepath.Join(t.TempDir(), ".agent00", "config.json"),
		required:           true,
		updatedAt:          time.Now().UTC(),
	}

	status, err := g.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if status.Ready || status.State != "needs_configure" {
		t.Fatalf("Verify() = %+v, want needs_configure", status)
	}
}

func TestStartDeviceAuthBlockedByMissingGitHubToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	g := &codexAuthGate{
		runner:             &authGateRunnerStub{},
		command:            "codex",
		requireGitHubToken: true,
		runtimeConfigPath:  filepath.Join(t.TempDir(), ".agent00", "config.json"),
		required:           true,
		updatedAt:          time.Now().UTC(),
	}

	status, err := g.StartDeviceAuth(context.Background())
	if err == nil {
		t.Fatal("StartDeviceAuth() error = nil, want github token required error")
	}
	if status.State != "needs_configure" {
		t.Fatalf("StartDeviceAuth() = %+v, want needs_configure", status)
	}
}

func TestStartDeviceAuthAcceptsNilContextAndMissingBaseContext(t *testing.T) {
	t.Parallel()

	g := &codexAuthGate{
		command:   "true",
		required:  true,
		state:     "needs_device_auth",
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
	}

	//nolint:staticcheck // intentionally exercising the nil-context fallback path
	status, err := g.StartDeviceAuth(nil)
	if err != nil {
		t.Fatalf("StartDeviceAuth(nil ctx) error = %v", err)
	}
	if status.State != "pending_device_auth" {
		t.Fatalf("StartDeviceAuth(nil ctx) = %+v", status)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return s.Ready && s.State == "ready"
	})
}

func TestStartDeviceAuthFailsWhenCodexHomeUnresolvable(t *testing.T) {
	previousCodexHome, hadCodexHome := os.LookupEnv("CODEX_HOME")
	previousHome, hadHome := os.LookupEnv("HOME")
	_ = os.Unsetenv("CODEX_HOME")
	_ = os.Unsetenv("HOME")
	t.Cleanup(func() {
		if hadCodexHome {
			_ = os.Setenv("CODEX_HOME", previousCodexHome)
		} else {
			_ = os.Unsetenv("CODEX_HOME")
		}
		if hadHome {
			_ = os.Setenv("HOME", previousHome)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})

	g := &codexAuthGate{
		baseCtx:   context.Background(),
		command:   "true",
		required:  true,
		state:     "needs_device_auth",
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
	}

	_, err := g.StartDeviceAuth(context.Background())
	if err == nil {
		t.Fatal("StartDeviceAuth() error = nil, want CODEX_HOME/HOME resolution failure")
	}
	if !strings.Contains(err.Error(), "CODEX_HOME or HOME is required") {
		t.Fatalf("error = %v, want CODEX_HOME/HOME requirement error", err)
	}
}

func TestStartDeviceAuthFailsWhenSessionLockCannotBeAcquired(t *testing.T) {
	// parentDir exists but is read-only, so the auth-marker lookup for the
	// not-yet-created CODEX_HOME directory legitimately resolves to "not
	// found" (letting updateAuthenticationFailure succeed), while
	// AcquireCodexSession's later os.MkdirAll fails with permission denied.
	parentDir := filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(parentDir) error = %v", err)
	}
	if err := os.Chmod(parentDir, 0o500); err != nil {
		t.Fatalf("Chmod(parentDir) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parentDir, 0o755) })
	codexHome := filepath.Join(parentDir, "codex-home")
	t.Setenv("CODEX_HOME", codexHome)

	g := &codexAuthGate{
		baseCtx:   context.Background(),
		command:   "true",
		required:  true,
		state:     "needs_device_auth",
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
	}

	_, err := g.StartDeviceAuth(context.Background())
	if err == nil {
		t.Fatal("StartDeviceAuth() error = nil, want session lock acquisition failure")
	}
	if !strings.Contains(err.Error(), "wait for Codex login session") {
		t.Fatalf("error = %v, want wrapped session-lock error", err)
	}
}

func TestStartDeviceAuthFallsBackToBackgroundContextWhenBaseContextNil(t *testing.T) {
	t.Parallel()

	g := &codexAuthGate{
		command:   "true",
		required:  true,
		state:     "needs_device_auth",
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
	}

	status, err := g.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}
	if status.State != "pending_device_auth" {
		t.Fatalf("status = %+v", status)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return s.Ready && s.State == "ready"
	})
}

func TestWaitDeviceAuthNoopsWhenAlreadyReady(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	g := &codexAuthGate{
		required:  true,
		ready:     true,
		state:     "ready",
		message:   "already ready",
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
	}
	before := g.updatedAt
	g.waitDeviceAuth(cmd, t.TempDir())

	if g.message != "already ready" {
		t.Fatalf("message = %q, want unchanged already ready", g.message)
	}
	if !g.updatedAt.Equal(before) {
		t.Fatalf("updatedAt changed for already-ready completion: before=%s after=%s", before, g.updatedAt)
	}
}

func TestWaitDeviceAuthReportsErrorWhenClearingAuthMarkerFails(t *testing.T) {
	home := t.TempDir()
	markerPath := filepath.Join(home, ".agent00-auth-required")
	if err := os.WriteFile(markerPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(marker) error = %v", err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatalf("Chmod(home) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	g := &codexAuthGate{
		required:    true,
		state:       "pending_device_auth",
		procRunning: true,
		updatedAt:   time.Now().UTC(),
		initCfg:     codexGitHubReadyInitCfg(),
	}
	g.waitDeviceAuth(cmd, t.TempDir())

	status, _ := g.Status(context.Background())
	if status.Ready || status.State != "error" {
		t.Fatalf("status = %+v, want error state", status)
	}
	if !strings.Contains(status.Message, "could not clear authentication failure state") {
		t.Fatalf("message = %q, want clear-failure detail", status.Message)
	}
}

func TestNewCodexAuthGateLogsWarnWhenConstructorAutoStartFails(t *testing.T) {
	t.Parallel()

	var logged []string
	runner := &authGateRunnerStub{run: func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if isGitHubTokenValidationCommandForTest(cmd) {
			return execx.Result{Stdout: "github.com logged in"}, nil
		}
		return execx.Result{}, errors.New("not logged in")
	}}

	missingCommand := filepath.Join(t.TempDir(), "missing-codex-binary")
	_ = newCodexAuthGateWithConfig(context.Background(), runner, missingCommand, "", codexGitHubReadyInitCfg(), true, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})

	if len(logged) == 0 {
		t.Fatal("expected warn log when constructor auto-start device auth fails")
	}
}

func TestCodexAuthGateStatusPropagatesProbeErrorWhenNeedsConfigureRecheck(t *testing.T) {
	t.Parallel()

	runner := &authGateRunnerStub{run: func(_ context.Context, _ execx.Command) (execx.Result, error) {
		return execx.Result{}, errors.New("command not found")
	}}
	g := &codexAuthGate{
		runner:    runner,
		command:   "codex",
		required:  true,
		state:     "needs_configure",
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
	}

	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v, want nil (probe errors surface via state)", err)
	}
	if status.Ready || status.State != "error" {
		t.Fatalf("Status() = %+v, want ready=false state=error after failed recheck", status)
	}
	if !strings.Contains(status.Message, "codex CLI is not available on PATH") {
		t.Fatalf("Status() message = %q, want probe error detail", status.Message)
	}
}

func TestProbeFailsWhenTempDirCannotBeCreated(t *testing.T) {
	badTempRoot := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badTempRoot, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("TMPDIR", badTempRoot)
	t.Setenv("CODEX_HOME", t.TempDir())

	g := &codexAuthGate{runner: &authGateRunnerStub{}, command: "codex"}
	ready, msg, err := g.probe(context.Background())
	if err == nil || ready || msg != "" {
		t.Fatalf("probe() = (%v, %q, %v), want temp dir creation failure", ready, msg, err)
	}
	if !strings.Contains(err.Error(), "create auth probe dir") {
		t.Fatalf("error = %v, want wrapped temp dir error", err)
	}
}

func TestProbeClassifiesRefreshTokenFailureWithoutNotLoggedInPhrase(t *testing.T) {
	t.Parallel()

	runner := &authGateRunnerStub{run: func(_ context.Context, _ execx.Command) (execx.Result, error) {
		return execx.Result{}, errors.New("refresh_token_expired, please re-authenticate")
	}}
	g := &codexAuthGate{runner: runner, command: "codex"}

	ready, msg, err := g.probe(context.Background())
	if err != nil || ready {
		t.Fatalf("probe() = (%v, %q, %v), want auth-required classification", ready, msg, err)
	}
	if got, want := msg, agentruntime.ErrCodexAuthRequired.Error(); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestCodexAuthGateStatusLogsWarnWhenAutoStartDeviceAuthFails(t *testing.T) {
	badTempRoot := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badTempRoot, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("TMPDIR", badTempRoot)

	var logged []string
	g := &codexAuthGate{
		baseCtx:   context.Background(),
		command:   "true",
		required:  true,
		state:     "needs_device_auth",
		message:   "Codex is not logged in. Device authorization is required.",
		updatedAt: time.Now().UTC(),
		initCfg:   codexGitHubReadyInitCfg(),
		logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}

	if _, err := g.Status(context.Background()); err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(logged) == 0 {
		t.Fatal("expected warn log when Status() auto-start device auth fails")
	}
}
