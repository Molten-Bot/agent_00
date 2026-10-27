package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestClaudeBrowserReauthenticationReplacesRejectedToken(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv(claudeOAuthTokenEnv, "expired-token-test-only")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := localruntime.SaveRuntimeConfigClaudeOAuthToken(path, localruntime.InitConfig{}, "expired-token-test-only"); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "claude-login.sh")
	if err := os.WriteFile(command, []byte(`#!/bin/sh
if [ "$1 $2" = 'auth status' ]; then exit 1; fi
echo 'https://claude.com/cai/oauth/authorize?code=true&state=test-state'
read advance
read code
[ "$code" = 'browser-code#test-state' ] || exit 65
mkdir -p "$HOME/.claude"
printf '{"claudeAiOauth":{"accessToken":"fresh-session-test-only","refreshToken":"refresh-test-only"}}' > "$HOME/.claude/.credentials.json"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newClaudeAuthGateWithContextAndConfig(ctx, command, path, localruntime.InitConfig{}, nil)
	if state, err := g.Status(ctx); err != nil || !state.Ready {
		t.Fatalf("stored token should initially appear ready: %+v, %v", state, err)
	}
	if state, err := g.Reauthenticate(ctx); err != nil || state.Ready {
		t.Fatalf("reauthenticate: %+v, %v", state, err)
	}
	if _, err := g.Reauthenticate(ctx); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		state, _ := g.Status(ctx)
		return state.AuthURL != ""
	})
	if _, err := g.Configure(ctx, "browser-code#test-state"); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.procRunning
	})
	state, err := g.Status(ctx)
	if err != nil || !state.Ready {
		t.Fatalf("successful browser reauthentication remains blocked: ready=%v, state=%s, err=%v", state.Ready, state.State, err)
	}
	if token, _ := firstConfiguredClaudeOAuthToken(path); token != "" {
		t.Fatal("rejected token still overrides the new browser login")
	}
	restarted := newClaudeAuthGateWithContextAndConfig(ctx, command, path, localruntime.InitConfig{}, nil)
	state, err = restarted.Status(ctx)
	if err != nil || !state.Ready {
		t.Fatalf("browser login not preserved across restart: ready=%v, err=%v", state.Ready, err)
	}
}

func TestClaudeImportedCredentialsKeepRefreshableSession(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv(claudeOAuthTokenEnv, "expired-token-test-only")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := localruntime.SaveRuntimeConfigClaudeOAuthToken(path, localruntime.InitConfig{}, "expired-token-test-only"); err != nil {
		t.Fatal(err)
	}
	g := newClaudeAuthGateWithConfig(filepath.Join(t.TempDir(), "missing-claude"), path, localruntime.InitConfig{})
	g.RequireReauthentication()
	raw := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test-only","refreshToken":"refresh-test-only","expiresAt":1}}`
	state, err := g.Configure(context.Background(), raw)
	if err != nil || !state.Ready {
		t.Fatalf("import: ready=%v, err=%v", state.Ready, err)
	}
	if token, _ := firstConfiguredClaudeOAuthToken(path); token != "" {
		t.Fatal("imported access token overrides the refreshable credential file")
	}
	data, err := os.ReadFile(claudeCredentialsPath())
	if err != nil || strings.TrimSpace(string(data)) != raw {
		t.Fatal("import did not preserve the complete refreshable session")
	}
}

func TestClaudeFailedBrowserLoginPreservesToken(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")
	t.Setenv(claudeOAuthTokenEnv, "expired-token-test-only")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := localruntime.SaveRuntimeConfigClaudeOAuthToken(path, localruntime.InitConfig{}, "expired-token-test-only"); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "claude-login.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := newClaudeAuthGateWithContextAndConfig(context.Background(), command, path, localruntime.InitConfig{}, nil)
	g.RequireReauthentication()
	if _, err := g.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.procRunning
	})
	state, err := g.Status(context.Background())
	if err != nil || state.Ready {
		t.Fatalf("failed login: ready=%v, err=%v", state.Ready, err)
	}
	if os.Getenv(claudeOAuthTokenEnv) != "expired-token-test-only" || localruntime.ReadRuntimeConfigString(path, "claude_code_oauth_token") != "expired-token-test-only" {
		t.Fatal("failed login discarded the previous token")
	}
	// Explicit setup-token input must continue to work after a failed login.
	state, err = g.Configure(context.Background(), "sk-ant-oat01-new-test-only")
	if err != nil || !state.Ready {
		t.Fatalf("explicit token replacement: ready=%v, err=%v", state.Ready, err)
	}
	if os.Getenv(claudeOAuthTokenEnv) != "sk-ant-oat01-new-test-only" || localruntime.ReadRuntimeConfigString(path, "claude_code_oauth_token") != "sk-ant-oat01-new-test-only" {
		t.Fatal("explicit token replacement was not preserved")
	}
}

func TestClaudeRejectedCredentialsRequireFreshLogin(t *testing.T) {
	for _, source := range []string{"token", "file"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("GH_TOKEN", "github_token_ready")
			writeCredentials := func(value string) {
				if source == "token" {
					t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", value)
				} else if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeCredentials("expired-credential")
			gate := newClaudeAuthGate(context.Background(), filepath.Join(t.TempDir(), "missing-claude"), nil)
			assertReady := func(want bool) {
				t.Helper()
				state, err := gate.Status(context.Background())
				if err != nil || state.Ready != want {
					t.Fatalf("Status() = %+v, %v; want ready=%v", state, err, want)
				}
				if !want && (state.State != "needs_browser_login" || !strings.Contains(state.Message, "rerun")) {
					t.Fatalf("missing reauthentication guidance: %+v", state)
				}
			}
			assertReady(true)
			selectable := &selectableAgentAuthGate{delegate: gate}
			selectable.RequireReauthentication()
			assertReady(false)
			assertReady(false) // Polling must not accept the same rejected credentials.
			writeCredentials("fresh-credential")
			assertReady(true)
		})
	}
}

func TestClaudeAuthenticationFailureDoesNotQueueCodeRepair(t *testing.T) {
	t.Parallel()
	result := app.Result{ExitCode: app.ExitCodex, Err: fmt.Errorf("agent failed: %w", agentruntime.ErrClaudeAuthRequired)}
	if queue, reason := shouldQueueFailureFollowUp(localSubmitSource, result); queue || !strings.Contains(reason, "sign-in") {
		t.Fatalf("follow-up = %v, %q; want sign-in required", queue, reason)
	}
}
