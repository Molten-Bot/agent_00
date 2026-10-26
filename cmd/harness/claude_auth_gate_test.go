package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
)

func TestClaudeAuthGateRequiresGitHubConfigureWhenTokenIsMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	g := newClaudeAuthGate(context.Background(), "", nil)
	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Harness != agentruntime.HarnessClaude || status.Ready || !status.Required {
		t.Fatalf("status = %+v", status)
	}
	if got, want := status.State, "needs_configure"; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
	if got, want := status.ConfigureCommand, claudeGitHubConfigureCommand; got != want {
		t.Fatalf("ConfigureCommand = %q, want %q", got, want)
	}
	if got, want := status.ConfigurePlaceholder, claudeGitHubConfigurePlaceholder; got != want {
		t.Fatalf("ConfigurePlaceholder = %q, want %q", got, want)
	}
	if !strings.Contains(status.Message, "GitHub token is required") {
		t.Fatalf("message = %q", status.Message)
	}
}

func TestClaudeAuthGateRequiresBrowserLoginWhenClaudeCredentialsAreMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	g := newClaudeAuthGate(context.Background(), "", nil)
	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got, want := status.State, "needs_browser_login"; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
	if got, want := status.ConfigureCommand, ""; got != want {
		t.Fatalf("ConfigureCommand = %q, want %q", got, want)
	}
	if got, want := status.ConfigurePlaceholder, ""; got != want {
		t.Fatalf("ConfigurePlaceholder = %q, want %q", got, want)
	}
	if got := status.AuthURL; got != "" {
		t.Fatalf("AuthURL = %q, want empty until login command emits a browser URL", got)
	}
	if !strings.Contains(status.Message, "Sign in to Claude") {
		t.Fatalf("message = %q", status.Message)
	}
}

func TestClaudeAuthGateRequiresClaudeCredentialsAfterValidatedGITHUBTOKEN(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "github_token_ready")

	runner := &authGateRunnerStub{
		run: func(_ context.Context, cmd execx.Command) (execx.Result, error) {
			if got, want := cmd.Name, "gh"; got != want {
				t.Fatalf("command = %q, want %q", got, want)
			}
			if got, want := strings.Join(cmd.Args, " "), gitHubTokenValidationArgsStringForTest(); got != want {
				t.Fatalf("args = %q, want %q", got, want)
			}
			if got, want := os.Getenv("GITHUB_TOKEN"), "github_token_ready"; got != want {
				t.Fatalf("GITHUB_TOKEN = %q, want %q during validation", got, want)
			}
			return execx.Result{Stdout: "github.com logged in"}, nil
		},
	}

	g := newClaudeAuthGateWithContextAndConfigAndRunner(context.Background(), runner, "", "", localruntime.InitConfig{}, nil)
	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got, want := status.State, "needs_browser_login"; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
	if got, want := status.ConfigureCommand, ""; got != want {
		t.Fatalf("ConfigureCommand = %q, want %q", got, want)
	}
	if got := status.ConfigureCommand; got == claudeGitHubConfigureCommand {
		t.Fatalf("ConfigureCommand = %q, want Claude credentials flow after GitHub validation", got)
	}
}

func TestClaudeAuthGateRecognizesEnvironmentCredentials(t *testing.T) {
	t.Run("api-key", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "github_token_ready")
		t.Setenv("ANTHROPIC_API_KEY", "test-key")
		g := newClaudeAuthGate(context.Background(), "", nil)
		status, err := g.Verify(context.Background())
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if !status.Ready {
			t.Fatalf("status = %+v", status)
		}
		if got, want := status.Message, "Claude Code and GitHub token are ready."; got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
	})

	t.Run("auth-token", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "github_token_ready")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "token")
		g := newClaudeAuthGate(context.Background(), "", nil)
		status, err := g.Verify(context.Background())
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if !status.Ready {
			t.Fatalf("status = %+v", status)
		}
		if got, want := status.Message, "Claude Code and GitHub token are ready."; got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
	})

	t.Run("cloud-provider", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "github_token_ready")
		t.Setenv("CLAUDE_CODE_USE_BEDROCK", "true")
		g := newClaudeAuthGate(context.Background(), "", nil)
		status, err := g.Verify(context.Background())
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if !status.Ready {
			t.Fatalf("status = %+v", status)
		}
		if got, want := status.Message, "Claude Code and GitHub token are ready."; got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
	})
}

func TestClaudeAuthGateRecognizesCredentialFile(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	credentialsPath := filepath.Join(configDir, ".credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"token":"abc"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("GH_TOKEN", "github_token_ready")

	g := newClaudeAuthGate(context.Background(), "", nil)
	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("status = %+v", status)
	}
	if got, want := status.Message, "Claude Code and GitHub token are ready."; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestClaudeAuthGateRecognizesCredentialFileFromHomeConfigDirectory(t *testing.T) {
	homeDir := t.TempDir()
	credentialsPath := filepath.Join(homeDir, ".config", "claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(credentialsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(credentialsPath, []byte(`{"token":"abc"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("HOME", homeDir)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	g := newClaudeAuthGate(context.Background(), "", nil)
	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("status = %+v", status)
	}
	if got, want := status.Message, "Claude Code and GitHub token are ready."; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestClaudeAuthGateRecognizesCLIAuthStatusWhenLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	cmdPath := filepath.Join(t.TempDir(), "claude-auth-status-ready.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  echo '{"loggedIn":true,"authMethod":"oauth","apiProvider":"firstParty"}'
  exit 0
fi
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := newClaudeAuthGate(context.Background(), cmdPath, nil)
	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("status = %+v", status)
	}
	if !strings.Contains(status.Message, "ready") {
		t.Fatalf("message = %q, want readiness message", status.Message)
	}
}

func clearClaudeCredentialSignals(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv(claudeOAuthTokenEnv, "")
}

func TestClaudeAuthGateConfigurePersistsGitHubTokenAndEnvironment(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	useSuccessfulGitHubStarTestServer(t)

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	g := newClaudeAuthGateWithConfig("", path, localruntime.InitConfig{

		AgentHarness: agentruntime.HarnessClaude,
	})

	githubToken := fakeGitHubPAT("saved_token")
	status, err := g.Configure(context.Background(), githubToken)
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if got, want := status.State, "needs_browser_login"; got != want {
		t.Fatalf("State = %q, want %q (Claude credentials are configured separately)", got, want)
	}
	if got, want := status.ConfigureCommand, ""; got != want {
		t.Fatalf("ConfigureCommand = %q, want %q", got, want)
	}
	if got, want := os.Getenv("GH_TOKEN"), githubToken; got != want {
		t.Fatalf("GH_TOKEN = %q, want %q", got, want)
	}
	if got, want := os.Getenv("GITHUB_TOKEN"), githubToken; got != want {
		t.Fatalf("GITHUB_TOKEN = %q, want %q", got, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got, want := doc["github_token"], githubToken; got != want {
		t.Fatalf("github_token = %#v, want %q", got, want)
	}
}

func TestClaudeAuthGateConfigureAcceptsCredentialsJSONInConfigureState(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv("GITHUB_TOKEN", "github_token_ready")
	t.Setenv(claudeOAuthTokenEnv, "")
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	g := &claudeAuthGate{
		baseCtx:           context.Background(),
		required:          true,
		state:             "needs_configure",
		message:           "auth pending",
		runtimeConfigPath: path,
		initCfg: localruntime.InitConfig{

			AgentHarness: agentruntime.HarnessClaude,
			GitHubToken:  "github_token_ready",
		},
		updatedAt: time.Now().UTC(),
		logf:      func(string, ...any) {},
	}

	claudeOAuthToken := "sk-" + "ant-oat01-manual-token-123456"
	raw := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, claudeOAuthToken)
	status, err := g.Configure(context.Background(), raw)
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("status = %+v, want ready", status)
	}
	if os.Getenv(claudeOAuthTokenEnv) != "" {
		t.Fatal("credentials JSON must not become an environment token override")
	}

	if token, _ := firstConfiguredClaudeOAuthToken(path); token != "" {
		t.Fatal("credentials JSON must not become a persisted token override")
	}

	credentialsPath := filepath.Join(claudeConfigDir, ".credentials.json")
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", credentialsPath, err)
	}
	if got, want := strings.TrimSpace(string(data)), raw; got != want {
		t.Fatalf("credentials json = %q, want %q", got, want)
	}
}

func TestClaudeAuthGateConfigureAcceptsUnknownCredentialsJSONSchemaInConfigureState(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv("GITHUB_TOKEN", "github_token_ready")
	t.Setenv(claudeOAuthTokenEnv, "")
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)

	g := &claudeAuthGate{
		baseCtx:  context.Background(),
		required: true,
		state:    "needs_configure",
		message:  "auth pending",
		initCfg: localruntime.InitConfig{

			AgentHarness: agentruntime.HarnessClaude,
			GitHubToken:  "github_token_ready",
		},
		updatedAt: time.Now().UTC(),
		logf:      func(string, ...any) {},
	}

	raw := `{"futureSchema":{"oauth":{"tokenEnvelope":{"value":"opaque"}}},"meta":{"version":2}}`
	status, err := g.Configure(context.Background(), raw)
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("status = %+v, want ready", status)
	}
	if got := os.Getenv(claudeOAuthTokenEnv); got != "" {
		t.Fatalf("%s = %q, want empty", claudeOAuthTokenEnv, got)
	}

	credentialsPath := filepath.Join(claudeConfigDir, ".credentials.json")
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", credentialsPath, err)
	}
	if got, want := strings.TrimSpace(string(data)), raw; got != want {
		t.Fatalf("credentials json = %q, want %q", got, want)
	}
}

func TestClaudeAuthGateConfigureRejectsInvalidCredentialsJSONInConfigureState(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv("GITHUB_TOKEN", "github_token_ready")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	g := &claudeAuthGate{
		baseCtx:  context.Background(),
		required: true,
		state:    "needs_configure",
		message:  "auth pending",
		initCfg: localruntime.InitConfig{

			AgentHarness: agentruntime.HarnessClaude,
			GitHubToken:  "github_token_ready",
		},
		updatedAt: time.Now().UTC(),
		logf:      func(string, ...any) {},
	}

	status, err := g.Configure(context.Background(), `{"claudeAiOauth":`)
	if err == nil {
		t.Fatal("Configure() error = nil, want non-nil")
	}
	if got := status.State; got != "needs_browser_login" {
		t.Fatalf("status.State = %q, want %q", got, "needs_browser_login")
	}
	if !strings.Contains(status.Message, "Claude credentials JSON is invalid:") {
		t.Fatalf("status.Message = %q, want invalid JSON details", status.Message)
	}
	if !strings.Contains(err.Error(), "invalid claude credentials json") {
		t.Fatalf("Configure() error = %q, want invalid claude credentials json", err)
	}
}

func TestClaudeAuthGatePendingBrowserLoginFailureNilGate(t *testing.T) {
	var g *claudeAuthGate

	wantErr := context.DeadlineExceeded
	status, err := g.pendingBrowserLoginFailure("ignored", wantErr)
	if err != wantErr {
		t.Fatalf("pendingBrowserLoginFailure() error = %v, want %v", err, wantErr)
	}
	if !status.Ready || status.State != "ready" {
		t.Fatalf("pendingBrowserLoginFailure() status = %+v", status)
	}
}

func TestClaudeAuthGateStartDeviceAuthRunsLoginAndCapturesURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	cmdPath := filepath.Join(t.TempDir(), "claude-login-stub.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  echo "unexpected args: $*" >&2
  exit 64
fi
echo "Select login method:"
if ! read choice; then
  echo "read failed" >&2
  exit 2
fi
echo "Open browser:"
echo "https://claude.ai/login/device?flow=test"
sleep 0.1
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := &claudeAuthGate{
		baseCtx:   context.Background(),
		command:   cmdPath,
		required:  true,
		state:     "needs_browser_login",
		message:   "auth required",
		updatedAt: time.Now().UTC(),
		logf:      func(string, ...any) {},
	}

	status, err := g.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}
	if status.State != "pending_browser_login" {
		t.Fatalf("initial status = %+v", status)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "https://claude.ai/login/device?flow=test")
	})

	status, err = g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got, want := status.AuthURL, "https://claude.ai/login/device?flow=test"; got != want {
		t.Fatalf("AuthURL = %q, want %q", got, want)
	}
	if status.Ready {
		t.Fatalf("status = %+v, want not ready", status)
	}
	if got, want := status.State, "pending_browser_login"; got != want {
		t.Fatalf("State = %q, want %q", got, want)
	}
}

func TestClaudeAuthGateStartDeviceAuthDefaultsNilBaseContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	cmdPath := filepath.Join(t.TempDir(), "claude-login-nil-ctx.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
echo "Select login method:"
if ! read choice; then
  exit 2
fi
echo "Open browser:"
echo "https://claude.ai/login/device?flow=nil-ctx"
sleep 0.1
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// baseCtx is intentionally left nil to exercise StartDeviceAuth's fallback
	// to context.Background() when constructed without the usual helpers.
	g := &claudeAuthGate{
		command:   cmdPath,
		required:  true,
		state:     "needs_browser_login",
		message:   "auth required",
		updatedAt: time.Now().UTC(),
		logf:      func(string, ...any) {},
	}

	status, err := g.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}
	if status.State != "pending_browser_login" {
		t.Fatalf("initial status = %+v", status)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "https://claude.ai/login/device?flow=nil-ctx")
	})
}

func TestClaudeAuthGateVerifyStartsBrowserLogin(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv("GITHUB_TOKEN", "github_token_ready")
	cmdPath := filepath.Join(t.TempDir(), "claude-login.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  exit 1
fi
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
echo 'https://claude.com/cai/oauth/authorize?code=true&state=test-state'
read advance
read code
[ "$code" = 'browser-code#test-state' ] || exit 65
mkdir -p "$HOME/.claude"
printf '{"claudeAiOauth":{"accessToken":"test-session"}}' > "$HOME/.claude/.credentials.json"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newClaudeAuthGate(ctx, cmdPath, nil)
	status, err := g.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "pending_browser_login" || status.Ready {
		t.Fatalf("Verify() = %+v, want pending browser login", status)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		status, _ = g.Status(ctx)
		return status.AuthURL != "" && status.AcceptsBrowserCode
	})
	if _, err := g.Configure(ctx, "browser-code#test-state"); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		status, _ = g.Status(ctx)
		return status.Ready
	})
	// A new gate must recognize credentials saved in the container home.
	restarted := newClaudeAuthGate(ctx, cmdPath, nil)
	status, err = restarted.Status(ctx)
	if err != nil || !status.Ready {
		t.Fatalf("restarted Status() = %+v, %v", status, err)
	}
}

func TestClaudeAuthGateStartDeviceAuthSendsInitialPromptAdvance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	cmdPath := filepath.Join(t.TempDir(), "claude-login-initial-enter.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
if ! read choice; then
  echo "read failed" >&2
  exit 3
fi
echo "https://claude.ai/login/device?flow=initial-enter"
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := &claudeAuthGate{
		baseCtx:   context.Background(),
		command:   cmdPath,
		required:  true,
		state:     "needs_browser_login",
		message:   "auth required",
		updatedAt: time.Now().UTC(),
		logf:      func(string, ...any) {},
	}

	if _, err := g.StartDeviceAuth(context.Background()); err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "https://claude.ai/login/device?flow=initial-enter")
	})
}

func TestClaudeAuthGateConfigureSubmitsBrowserCodeToRunningLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	capturedPath := filepath.Join(t.TempDir(), "captured-code.txt")
	cmdPath := filepath.Join(t.TempDir(), "claude-login-submit-code.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
echo "Select login method:"
if ! read choice; then
  exit 2
fi
echo "Open browser:"
echo "https://claude.ai/login/device?flow=submit-code"
if ! read authcode; then
  exit 3
fi
printf "%s" "$authcode" > "`+capturedPath+`"
sleep 1
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := newClaudeAuthGate(context.Background(), cmdPath, nil)
	if _, err := g.StartDeviceAuth(context.Background()); err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "https://claude.ai/login/device?flow=submit-code")
	})

	const submittedCode = "7zhfhHRjHhqZKEwd0T3hN4npff2bJOBx0NJYCWSggMrzXlTi#O_nyYiogHRbCqVf0kOk0oSWejds77rLgKVbzVCcenKQ"
	status, err := g.Configure(context.Background(), submittedCode)
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if got, want := status.State, "pending_browser_login"; got != want {
		t.Fatalf("Configure() state = %q, want %q", got, want)
	}
	if !strings.Contains(strings.ToLower(status.Message), "credentials.json") {
		t.Fatalf("Configure() message = %q, want fallback credentials hint", status.Message)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(capturedPath)
		if err != nil {
			return false
		}
		return string(data) == submittedCode
	})
}

func TestClaudeAuthGateConfigureNormalizesBrowserCodeWhitespace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	capturedPath := filepath.Join(t.TempDir(), "captured-code-normalized.txt")
	cmdPath := filepath.Join(t.TempDir(), "claude-login-submit-code-normalized.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
echo "Select login method:"
if ! read choice; then
  exit 2
fi
echo "Open browser:"
echo "https://claude.ai/login/device?flow=submit-code-normalized"
if ! read authcode; then
  exit 3
fi
printf "%s" "$authcode" > "`+capturedPath+`"
sleep 1
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := newClaudeAuthGate(context.Background(), cmdPath, nil)
	if _, err := g.StartDeviceAuth(context.Background()); err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "https://claude.ai/login/device?flow=submit-code-normalized")
	})

	rawPastedCode := "  code-from-\n  claude \t browser  "
	status, err := g.Configure(context.Background(), rawPastedCode)
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if got, want := status.State, "pending_browser_login"; got != want {
		t.Fatalf("Configure() state = %q, want %q", got, want)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(capturedPath)
		if err != nil {
			return false
		}
		return string(data) == "code-from-claudebrowser"
	})
}

func TestClaudeAuthGateConfigureAppendsOAuthStateWhenMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	capturedPath := filepath.Join(t.TempDir(), "captured-code-with-state.txt")
	cmdPath := filepath.Join(t.TempDir(), "claude-login-submit-code-with-state.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
echo "Select login method:"
if ! read choice; then
  exit 2
fi
echo "Open browser:"
echo "https://claude.ai/oauth/authorize?code=true&state=test-state-123"
if ! read authcode; then
  exit 3
fi
printf "%s" "$authcode" > "`+capturedPath+`"
sleep 1
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := newClaudeAuthGate(context.Background(), cmdPath, nil)
	if _, err := g.StartDeviceAuth(context.Background()); err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "state=test-state-123")
	})

	const submittedCode = "code-from-browser-only"
	if _, err := g.Configure(context.Background(), submittedCode); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(capturedPath)
		if err != nil {
			return false
		}
		return string(data) == submittedCode+"#test-state-123"
	})
}

func TestClaudeAuthGateVerifyResubmitsPendingBrowserCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	capturedPath := filepath.Join(t.TempDir(), "captured-resubmitted-code.txt")
	cmdPath := filepath.Join(t.TempDir(), "claude-login-resubmit-code.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
echo "Select login method:"
if ! read choice; then
  exit 2
fi
echo "Open browser:"
echo "https://claude.ai/oauth/authorize?code=true&state=resubmit-state"
if ! read discard; then
  exit 3
fi
if ! read authcode; then
  exit 4
fi
printf "%s" "$authcode" > "`+capturedPath+`"
sleep 1
exit 1
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := newClaudeAuthGate(context.Background(), cmdPath, nil)
	if _, err := g.StartDeviceAuth(context.Background()); err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "state=resubmit-state")
	})

	if _, err := g.Configure(context.Background(), "resubmit-code"); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	// Let resubmit throttle elapse, then verify to trigger resend while pending.
	time.Sleep(1300 * time.Millisecond)
	if _, err := g.Verify(context.Background()); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(capturedPath)
		if err != nil {
			return false
		}
		return string(data) == "resubmit-code#resubmit-state"
	})
}

func TestClaudeAuthGateVerifyCompletesWhenCredentialsAreReadyBeforeLoginExit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("GH_TOKEN", "github_token_ready")

	capturedPath := filepath.Join(t.TempDir(), "captured-code.txt")
	cmdPath := filepath.Join(t.TempDir(), "claude-login-verify-ready.sh")
	if err := os.WriteFile(cmdPath, []byte(`#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "login" ]; then
  exit 64
fi
echo "Choose account:"
if ! read choice; then
  exit 2
fi
echo "Continue at https://claude.ai/login/verify-ready"
if ! read authcode; then
  exit 3
fi
printf "%s" "$authcode" > "`+capturedPath+`"
mkdir -p "$CLAUDE_CONFIG_DIR"
printf '{"accessToken":"ok"}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
sleep 30
`), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := newClaudeAuthGate(baseCtx, cmdPath, nil)
	if _, err := g.StartDeviceAuth(context.Background()); err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		s, _ := g.Status(context.Background())
		return strings.Contains(s.AuthURL, "https://claude.ai/login/verify-ready")
	})

	const submittedCode = "auth-code-from-browser"
	if _, err := g.Configure(context.Background(), submittedCode); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(capturedPath)
		if err != nil {
			return false
		}
		return string(data) == submittedCode
	})

	var (
		status web.AgentAuthState
		err    error
	)
	waitForCondition(t, 5*time.Second, func() bool {
		status, err = g.Verify(context.Background())
		return err == nil && status.Ready
	})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("Verify() ready = false, want true; status=%+v", status)
	}
	if got, want := status.State, "ready"; got != want {
		t.Fatalf("Verify() state = %q, want %q", got, want)
	}
	if status.AuthURL != "" {
		t.Fatalf("Verify() authURL = %q, want empty", status.AuthURL)
	}
}

func TestClaudeAuthHelpers(t *testing.T) {
	t.Parallel()

	if got, want := normalizeClaudeBrowserCode("  code-from-\n claude \t browser  "), "code-from-claudebrowser"; got != want {
		t.Fatalf("normalizeClaudeBrowserCode() = %q, want %q", got, want)
	}
	jsonToken := "sk-" + "ant-oat01-json-token-123456"
	if got, want := extractClaudeOAuthTokenFromInput(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, jsonToken)), jsonToken; got != want {
		t.Fatalf("extractClaudeOAuthTokenFromInput(json) = %q, want %q", got, want)
	}
	directToken := "sk-" + "ant-oat01-direct-token-123456"
	if got, want := extractClaudeOAuthTokenFromInput(directToken), directToken; got != want {
		t.Fatalf("extractClaudeOAuthTokenFromInput(token) = %q, want %q", got, want)
	}
	if got := extractClaudeOAuthTokenFromInput("browserCodeWithoutPrefix#state-xyz"); got != "" {
		t.Fatalf("extractClaudeOAuthTokenFromInput(browser-code) = %q, want empty", got)
	}
	if got, want := extractClaudeAuthStateFromURL("https://claude.ai/oauth/authorize?code=true&state=abc123"), "abc123"; got != want {
		t.Fatalf("extractClaudeAuthStateFromURL() = %q, want %q", got, want)
	}
	if got, want := normalizeClaudeBrowserCodeForSubmission("code-only", "https://claude.ai/oauth/authorize?state=state-xyz"), "code-only#state-xyz"; got != want {
		t.Fatalf("normalizeClaudeBrowserCodeForSubmission(code-only) = %q, want %q", got, want)
	}
	if got, want := normalizeClaudeBrowserCodeForSubmission("https://platform.claude.com/oauth/code/callback?code=abc&state=xyz", ""), "abc#xyz"; got != want {
		t.Fatalf("normalizeClaudeBrowserCodeForSubmission(callback-url) = %q, want %q", got, want)
	}
	if got, want := normalizeClaudeBrowserCodeForSubmission("abc#xyz", "https://claude.ai/oauth/authorize?state=other"), "abc#xyz"; got != want {
		t.Fatalf("normalizeClaudeBrowserCodeForSubmission(existing-state) = %q, want %q", got, want)
	}
	if got, want := extractClaudeAuthURL("Use https://claude.ai/login/device?x=y); now"), "https://claude.ai/login/device?x=y"; got != want {
		t.Fatalf("extractClaudeAuthURL() = %q, want %q", got, want)
	}
	if got, want := extractClaudeAuthURL("If the browser didn't open, visit: https://claude.ai/oauth/authorize?code=true&client_id=abc123&scope=user%3Aprofile"), "https://claude.ai/oauth/authorize?code=true&client_id=abc123&scope=user%3Aprofile"; got != want {
		t.Fatalf("extractClaudeAuthURL() should parse oauth authorize URL: got %q, want %q", got, want)
	}
	if got, want := extractClaudeAuthURL("Read docs at https://code.claude.com/docs/en/authentication and sign in at https://claude.ai/login/device?flow=x"), "https://claude.ai/login/device?flow=x"; got != want {
		t.Fatalf("extractClaudeAuthURL() should ignore docs URL and keep login URL: got %q, want %q", got, want)
	}
	if got, want := extractClaudeAuthURL("\x1b]8;;https://claude.ai/login/device?flow=osc-test\x1b\\Authorize Claude\x1b]8;;\x1b\\"), "https://claude.ai/login/device?flow=osc-test"; got != want {
		t.Fatalf("extractClaudeAuthURL(osc-hyperlink) = %q, want %q", got, want)
	}
	if got := extractClaudeAuthURL("Read docs at https://code.claude.com/docs/en/authentication"); got != "" {
		t.Fatalf("extractClaudeAuthURL(docs-url-only) = %q, want empty", got)
	}
	if got := extractClaudeAuthURL("no url here"); got != "" {
		t.Fatalf("extractClaudeAuthURL(no-url) = %q, want empty", got)
	}

	for _, line := range []string{
		"Select login method:",
		"Choose account",
		"Press Enter to continue",
		"Which option would you like?",
	} {
		if !shouldAdvanceClaudeLoginPrompt(line) {
			t.Fatalf("shouldAdvanceClaudeLoginPrompt(%q) = false, want true", line)
		}
	}
	if shouldAdvanceClaudeLoginPrompt("Login URL: https://claude.ai/login") {
		t.Fatalf("shouldAdvanceClaudeLoginPrompt(url line) = true, want false")
	}
	for _, line := range []string{
		"Paste code here if prompted >",
		"Claude authentication code required",
		"if prompted >",
	} {
		if !shouldPromptForClaudeBrowserCode(line) {
			t.Fatalf("shouldPromptForClaudeBrowserCode(%q) = false, want true", line)
		}
	}
	if shouldPromptForClaudeBrowserCode("Open browser and continue") {
		t.Fatalf("shouldPromptForClaudeBrowserCode(non-prompt) = true, want false")
	}
	if !shouldCaptureClaudeOAuthToken("Your OAuth token (valid for 1 year):") {
		t.Fatalf("shouldCaptureClaudeOAuthToken() = false, want true")
	}
	for _, line := range []string{
		"Store this token securely.",
		"Use this token by setting CLAUDE_CODE_OAUTH_TOKEN.",
		"Login successful",
	} {
		if !shouldStopClaudeOAuthTokenCapture(line) {
			t.Fatalf("shouldStopClaudeOAuthTokenCapture(%q) = false, want true", line)
		}
	}
	for _, line := range []string{"", "Copy the token below"} {
		if shouldStopClaudeOAuthTokenCapture(line) {
			t.Fatalf("shouldStopClaudeOAuthTokenCapture(%q) = true, want false", line)
		}
	}
	if got, want := extractClaudeOAuthTokenCandidate("abc.DEF_123-xyz-token-value"), "abc.DEF_123-xyz-token-value"; got != want {
		t.Fatalf("extractClaudeOAuthTokenCandidate() = %q, want %q", got, want)
	}
	if got := extractClaudeOAuthTokenCandidate("https://claude.com/cai/oauth/authorize"); got != "" {
		t.Fatalf("extractClaudeOAuthTokenCandidate(url) = %q, want empty", got)
	}
	if got := claudeLoginArgs("claude"); len(got) != 2 || got[0] != "auth" || got[1] != "login" {
		t.Fatalf("claudeLoginArgs(claude) = %v, want [auth login]", got)
	}
	if got := claudeLoginArgs("/tmp/custom-claude-wrapper"); len(got) != 2 || got[0] != "auth" || got[1] != "login" {
		t.Fatalf("claudeLoginArgs(custom) = %v, want [auth login]", got)
	}
}

func TestAgentAuthGateFactorySelectsSupportedHarnesses(t *testing.T) {
	t.Parallel()

	if gate := newConcreteAgentAuthGate(context.Background(), nil, agentruntime.Runtime{Harness: agentruntime.HarnessCodex, Command: "codex"}, localruntime.InitConfig{}, nil); gate == nil {
		t.Fatal("newConcreteAgentAuthGate(codex) = nil")
	}
	if gate := newConcreteAgentAuthGate(context.Background(), nil, agentruntime.Runtime{Harness: agentruntime.HarnessClaude, Command: "claude"}, localruntime.InitConfig{}, nil); gate == nil {
		t.Fatal("newConcreteAgentAuthGate(claude) = nil")
	}
	if gate := newConcreteAgentAuthGate(context.Background(), nil, agentruntime.Runtime{Harness: "unknown"}, localruntime.InitConfig{}, nil); gate != nil {
		t.Fatalf("newConcreteAgentAuthGate(unknown) = %#v, want nil", gate)
	}
	if gate := newAgentAuthGate(context.Background(), nil, agentruntime.Runtime{Harness: "unknown"}, localruntime.InitConfig{}, nil); gate == nil {
		t.Fatal("newAgentAuthGate(unknown) = nil, want selectable wrapper")
	}
}

func TestExtractClaudeAuthCodeAndState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		wantCode  string
		wantState string
	}{
		{name: "empty", raw: "", wantCode: "", wantState: ""},
		{name: "whitespace only", raw: "   \t  ", wantCode: "", wantState: ""},
		{
			name:      "full callback url",
			raw:       "https://platform.claude.com/oauth/code/callback?code=abc123&state=xyz789",
			wantCode:  "abc123",
			wantState: "xyz789",
		},
		{
			name:      "bare query without scheme",
			raw:       "code=abc123&state=xyz789",
			wantCode:  "abc123",
			wantState: "xyz789",
		},
		{
			name:      "only equals sign, no ampersand",
			raw:       "code=abc123",
			wantCode:  "",
			wantState: "",
		},
		{
			name:      "only ampersand, no equals",
			raw:       "code&state",
			wantCode:  "",
			wantState: "",
		},
		{
			name:      "malformed query escape",
			raw:       "code=abc%zz&state=xyz",
			wantCode:  "",
			wantState: "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, state := extractClaudeAuthCodeAndState(tt.raw)
			if code != tt.wantCode || state != tt.wantState {
				t.Fatalf("extractClaudeAuthCodeAndState(%q) = (%q, %q), want (%q, %q)", tt.raw, code, state, tt.wantCode, tt.wantState)
			}
		})
	}
}

func TestSaveClaudeCredentialsJSONRejectsEmptyInput(t *testing.T) {
	t.Parallel()

	path, err := saveClaudeCredentialsJSON("   ")
	if err == nil || err.Error() != "claude credentials json is required" {
		t.Fatalf("saveClaudeCredentialsJSON(empty) error = %v, want required", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
}

func TestSaveClaudeCredentialsJSONRejectsUnavailablePath(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path, err := saveClaudeCredentialsJSON(`{"token":"abc"}`)
	if err == nil || !strings.Contains(err.Error(), "claude credentials path is unavailable") {
		t.Fatalf("saveClaudeCredentialsJSON(no home) error = %v, want path unavailable", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
}

func TestSaveClaudeCredentialsJSONReportsMkdirFailure(t *testing.T) {
	blockerFile := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocker) error = %v", err)
	}
	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", blockerFile)

	path, err := saveClaudeCredentialsJSON(`{"token":"abc"}`)
	if err == nil || !strings.Contains(err.Error(), "create claude credentials dir") {
		t.Fatalf("saveClaudeCredentialsJSON(blocked dir) error = %v, want mkdir failure", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
}

func TestSaveClaudeCredentialsJSONReportsWriteFailure(t *testing.T) {
	readOnlyDir := t.TempDir()
	if err := os.Chmod(readOnlyDir, 0o555); err != nil {
		t.Fatalf("Chmod(read-only) error = %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(readOnlyDir, 0o755)
	})

	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(readOnlyDir, "creds.json"))

	path, err := saveClaudeCredentialsJSON(`{"token":"abc"}`)
	if err == nil || !strings.Contains(err.Error(), "write claude credentials file") {
		t.Fatalf("saveClaudeCredentialsJSON(read-only dir) error = %v, want write failure", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
}

func TestSaveClaudeCredentialsJSONSucceeds(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	wantPath := filepath.Join(configDir, ".credentials.json")
	path, err := saveClaudeCredentialsJSON(`  {"token":"abc"}  `)
	if err != nil {
		t.Fatalf("saveClaudeCredentialsJSON() error = %v", err)
	}
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	data, readErr := os.ReadFile(wantPath)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if got, want := string(data), "{\"token\":\"abc\"}\n"; got != want {
		t.Fatalf("file contents = %q, want %q", got, want)
	}
}

func TestClaudeAuthGateSnapshotLockedDerivesStateAndForcesRequired(t *testing.T) {
	t.Parallel()

	readyGate := &claudeAuthGate{state: "", ready: true, required: false}
	readySnap := readyGate.snapshotLocked()
	if readySnap.State != "ready" {
		t.Fatalf("snapshotLocked(empty state, ready) State = %q, want ready", readySnap.State)
	}
	if !readySnap.Required {
		t.Fatal("snapshotLocked(empty state, ready) Required = false, want forced true")
	}

	notReadyGate := &claudeAuthGate{state: "", ready: false, required: false}
	notReadySnap := notReadyGate.snapshotLocked()
	if notReadySnap.State != "needs_configure" {
		t.Fatalf("snapshotLocked(empty state, not ready) State = %q, want needs_configure", notReadySnap.State)
	}
	if !notReadySnap.Required {
		t.Fatal("snapshotLocked(empty state, not ready) Required = false, want forced true")
	}
}

func TestClaudeAuthGateSubmitBrowserCodeNilGate(t *testing.T) {
	t.Parallel()

	var nilGate *claudeAuthGate
	state, err := nilGate.submitBrowserCode("some-code")
	if err != nil {
		t.Fatalf("submitBrowserCode(nil) error = %v", err)
	}
	if !state.Ready || state.State != "ready" {
		t.Fatalf("submitBrowserCode(nil) state = %+v, want ready", state)
	}
}

func TestClaudeAuthGateSubmitBrowserCodeRequiresNonEmptyCode(t *testing.T) {
	t.Parallel()

	g := &claudeAuthGate{state: "pending_browser_login"}
	state, err := g.submitBrowserCode("   ")
	if err == nil || err.Error() != "claude authentication code is required" {
		t.Fatalf("submitBrowserCode(empty) error = %v, want required", err)
	}
	if !strings.Contains(state.Message, "Claude authentication input is required") {
		t.Fatalf("state.Message = %q, want input-required hint", state.Message)
	}
}

func TestClaudeAuthGateSubmitBrowserCodeRequiresRunningLogin(t *testing.T) {
	t.Parallel()

	g := &claudeAuthGate{ready: false}
	state, err := g.submitBrowserCode("some-code")
	if err == nil || err.Error() != "claude login is not waiting for a code" {
		t.Fatalf("submitBrowserCode(no process) error = %v, want not-waiting", err)
	}
	if state.State != "needs_browser_login" {
		t.Fatalf("state.State = %q, want needs_browser_login", state.State)
	}
	if !strings.Contains(state.Message, "Click Done to restart browser login") {
		t.Fatalf("state.Message = %q, want restart hint", state.Message)
	}
}

type failingWriteCloser struct{}

func (failingWriteCloser) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (failingWriteCloser) Close() error              { return nil }

func TestClaudeAuthGateSubmitBrowserCodeHandlesWriteFailure(t *testing.T) {
	t.Parallel()

	g := &claudeAuthGate{
		ready:       false,
		procRunning: true,
		procInput:   failingWriteCloser{},
	}
	state, err := g.submitBrowserCode("some-code")
	if err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("submitBrowserCode(write failure) error = %v, want broken pipe", err)
	}
	if state.State != "pending_browser_login" {
		t.Fatalf("state.State = %q, want pending_browser_login", state.State)
	}
	if !strings.Contains(state.Message, "submit claude authentication code") {
		t.Fatalf("state.Message = %q, want submit failure detail", state.Message)
	}
}

func TestClaudeAuthGateStartDeviceAuthNilGate(t *testing.T) {
	t.Parallel()

	var nilGate *claudeAuthGate
	state, err := nilGate.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth(nil) error = %v", err)
	}
	if !state.Ready || state.State != "ready" {
		t.Fatalf("StartDeviceAuth(nil) state = %+v, want ready", state)
	}
}

func TestClaudeAuthGateStartDeviceAuthReturnsReadyStatusWithoutStartingProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")

	g := newClaudeAuthGate(context.Background(), "", nil)
	state, err := g.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}
	if !state.Ready {
		t.Fatalf("StartDeviceAuth() state = %+v, want ready", state)
	}
	if g.procRunning {
		t.Fatal("StartDeviceAuth() started a login process despite already-ready credentials")
	}
}

func TestClaudeAuthGateStartDeviceAuthRequiresGitHubToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	g := newClaudeAuthGate(context.Background(), "", nil)
	state, err := g.StartDeviceAuth(context.Background())
	if err == nil || err.Error() != "github token is required" {
		t.Fatalf("StartDeviceAuth(no github token) error = %v, want required", err)
	}
	if state.State != "needs_configure" || state.ConfigureCommand != claudeGitHubConfigureCommand {
		t.Fatalf("state = %+v, want needs_configure with github configure command", state)
	}
}

func TestClaudeAuthGateStartDeviceAuthReturnsPendingWhenProcessAlreadyRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")

	g := newClaudeAuthGate(context.Background(), filepath.Join(t.TempDir(), "nonexistent-claude-binary"), nil)

	g.mu.Lock()
	g.procRunning = true
	g.mu.Unlock()

	state, err := g.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth(already running) error = %v", err)
	}
	if state.State != "pending_browser_login" {
		t.Fatalf("state.State = %q, want pending_browser_login", state.State)
	}
	if !strings.Contains(state.Message, "Waiting for Claude browser sign-in") {
		t.Fatalf("state.Message = %q, want waiting hint", state.Message)
	}
}
