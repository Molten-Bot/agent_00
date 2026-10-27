package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestClaudeDiagnosticsReportsFailureWithoutSecrets(t *testing.T) {
	clearClaudeCredentialSignals(t)
	if _, err := saveClaudeCredentialsJSON(`{"claudeAiOauth":{"accessToken":"access-secret-test-only","refreshToken":"refresh-secret-test-only","expiresAt":1}}`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g := &claudeAuthGate{
		command: "claude-test", state: "needs_browser_login", authURL: "https://claude.ai/oauth?state=secret-state",
		pendingBrowserCode: "secret-browser-code", lastLoginErrorCode: "CLAUDE_BROWSER_CODE_INVALID",
		runner: &authGateRunnerStub{run: func(_ context.Context, cmd execx.Command) (execx.Result, error) {
			calls++
			if cmd.Name != "claude-test" || strings.Join(cmd.Args, " ") != "auth status" {
				t.Fatalf("unexpected probe: %+v", cmd)
			}
			return execx.Result{Stdout: `{"loggedIn":true,"email":"private-account@example.test","accessToken":"cli-secret-test-only"}`}, nil
		}},
	}
	g.RequireReauthentication()
	result, err := g.Diagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !result.CLI.StatusKnown || !result.CLI.LoggedIn || !result.Credentials.Expired || !result.Credentials.HasRefreshToken {
		t.Fatalf("diagnostics missing expected metadata: %+v", result)
	}
	codes := map[string]bool{}
	for _, check := range result.Checks {
		codes[check.Code] = true
	}
	for _, code := range []string{"CLAUDE_AUTH_REJECTED", "CLAUDE_ACCESS_TOKEN_EXPIRED", "CLAUDE_BROWSER_CODE_INVALID"} {
		if !codes[code] {
			t.Errorf("missing %s", code)
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-secret-test-only", "refresh-secret-test-only", "secret-state", "secret-browser-code", "private-account@example.test", "cli-secret-test-only"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("diagnostics leaked sensitive data")
		}
	}
}

func TestClaudeDiagnosticsDoesNotExposeProbeErrors(t *testing.T) {
	clearClaudeCredentialSignals(t)
	g := &claudeAuthGate{runner: &authGateRunnerStub{run: func(context.Context, execx.Command) (execx.Result, error) {
		return execx.Result{Stdout: "secret-stdout", Stderr: "secret-stderr"}, errors.New("secret-error")
	}}}
	result, err := g.Diagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "secret-") {
		t.Fatal("probe error or raw output leaked")
	}
	if len(result.Checks) != 1 || result.Checks[0].Code != "CLAUDE_STATUS_UNAVAILABLE" {
		t.Fatalf("unexpected diagnostics: %+v", result)
	}
}
