package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
)

func TestCodexGateInvalidatesReadyAfterRuntimeAuthenticationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	authPath := filepath.Join(home, "auth.json")
	if err := os.WriteFile(authPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &codexAuthGate{
		ready: true, state: "ready", required: true,
		runner: &authGateRunnerStub{run: func(_ context.Context, cmd execx.Command) (execx.Result, error) {
			return execx.Result{Stdout: "Logged in"}, nil
		}},
	}
	if err := agentruntime.MarkCodexAuthenticationRequired(home); err != nil {
		t.Fatal(err)
	}
	state, err := g.Status(context.Background())
	if err != nil || state.Ready || state.State != "needs_device_auth" {
		t.Fatalf("stale ready state = %+v, %v", state, err)
	}
	state, err = g.Verify(context.Background())
	if err != nil || state.Ready {
		t.Fatalf("local login status bypassed rejection: %+v, %v", state, err)
	}
	if err := os.WriteFile(authPath, []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err = g.Status(context.Background())
	if err != nil || !state.Ready {
		t.Fatalf("fresh login did not recover: %+v, %v", state, err)
	}
}

func TestRefreshTokenFailureNeverQueuesRepairTask(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		agentruntime.ErrCodexAuthRequired,
		errors.New("codex: exit status 1 (Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.)"),
		errors.New("refresh_token_reused"),
	} {
		if reason := failurefollowup.NonRemediableFailureReason(err); reason == "" {
			t.Errorf("auth failure is considered remediable: %v", err)
		}
		if queue, _ := shouldQueueFailureFollowUp(localSubmitSource, app.Result{ExitCode: app.ExitCodex, Err: err}); queue {
			t.Errorf("auth failure queued a repair: %v", err)
		}
	}
}

func TestDeviceLoginCompletionControlsBlockedCredentialsAndSessionLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		exit  string
		ready bool
	}{
		{"successful_login_clears_block", "0", true},
		{"failed_login_preserves_block", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			session, err := agentruntime.AcquireCodexSession(ctx, home)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Release()
			// No auth.json: also covers keyring-backed logins whose successful
			// completion must clear the block explicitly.
			if err := agentruntime.MarkCodexAuthenticationRequired(home); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", "exit "+tc.exit)
			cmd.Env = []string{"CODEX_HOME=" + home}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			gate := &codexAuthGate{required: true, authBlocked: true, procRunning: true,
				state: "pending_device_auth", procRelease: session.Release}
			gate.waitDeviceAuth(cmd, t.TempDir())
			if gate.ready != tc.ready || gate.procRunning {
				t.Fatalf("login completion state: ready=%t, running=%t", gate.ready, gate.procRunning)
			}
			err = agentruntime.CheckCodexAuthentication(home)
			if tc.ready && err != nil {
				t.Fatalf("successful login remained blocked: %v", err)
			}
			if !tc.ready && !errors.Is(err, agentruntime.ErrCodexAuthRequired) {
				t.Fatalf("failed login removed credential block: %v", err)
			}
			nextSession, err := agentruntime.AcquireCodexSession(ctx, home)
			if err != nil {
				t.Fatalf("login completion leaked session lock: %v", err)
			}
			nextSession.Release()
		})
	}
}
