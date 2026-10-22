package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/app"
)

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
