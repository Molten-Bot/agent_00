package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestCodexReauthenticateReplacesCachedLogin(t *testing.T) {
	for _, result := range []string{"success", "failure"} {
		t.Run(result, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			authPath := filepath.Join(home, "auth.json")
			if err := os.WriteFile(authPath, []byte("old-test-session"), 0o600); err != nil {
				t.Fatal(err)
			}
			command := filepath.Join(t.TempDir(), "codex-login.sh")
			script := `#!/bin/sh
echo 'https://auth.openai.com/device ABCD-EFGH'
while [ ! -f "$CODEX_HOME/finish" ]; do sleep 0.05; done
`
			if result == "success" {
				script += `printf fresh-test-session > "$CODEX_HOME/auth.json"` + "\n"
			} else {
				script += "exit 1\n"
			}
			if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := &authGateRunnerStub{run: func(context.Context, execx.Command) (execx.Result, error) {
				return execx.Result{Stdout: "Logged in"}, nil
			}}
			gate := newCodexAuthGate(ctx, runner, command, nil)
			if state, _ := gate.Status(ctx); !state.Ready {
				t.Fatal("cached login should initially appear ready")
			}
			for range 2 { // Repeated clicks reuse the pending login.
				state, err := gate.Reauthenticate(ctx)
				if err != nil || state.Ready || state.State != "pending_device_auth" {
					t.Fatalf("reauthenticate = %+v, %v", state, err)
				}
			}
			if state, err := gate.Verify(ctx); err != nil || state.Ready {
				t.Fatalf("cached login bypassed pending authentication: %+v, %v", state, err)
			}
			if err := os.WriteFile(filepath.Join(home, "finish"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			waitForCondition(t, 5*time.Second, func() bool {
				gate.mu.Lock()
				defer gate.mu.Unlock()
				return !gate.procRunning
			})
			state, err := gate.Verify(ctx)
			if err != nil || state.Ready != (result == "success") {
				t.Fatalf("completion = %+v, %v", state, err)
			}
			if result == "success" {
				restarted := newCodexAuthGate(ctx, runner, command, nil)
				if state, err := restarted.Status(ctx); err != nil || !state.Ready {
					t.Fatalf("fresh login did not survive restart: %+v, %v", state, err)
				}
			} else if err := agentruntime.CheckCodexAuthentication(home); !errors.Is(err, agentruntime.ErrCodexAuthRequired) {
				t.Fatalf("failed login accepted old credentials: %v", err)
			}
		})
	}
}
