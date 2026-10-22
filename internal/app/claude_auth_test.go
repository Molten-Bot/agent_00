package app

import (
	"context"
	"errors"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestClaudeInvocationAuthenticationFailure(t *testing.T) {
	t.Parallel()
	diagnostic := "Failed to authenticate. API Error: 401 OAuth access token has expired. Re-authenticate to continue."
	for _, tc := range []struct {
		name     string
		result   execx.Result
		err      error
		wantAuth bool
	}{
		{"stdout", execx.Result{Stdout: diagnostic}, errors.New("exit status 1"), true},
		{"stderr", execx.Result{Stderr: diagnostic}, errors.New("exit status 1"), true},
		{"successful recovery", execx.Result{Stdout: diagnostic}, nil, false},
		{"tool failure", execx.Result{Stderr: "curl: HTTP 401 Unauthorized"}, errors.New("exit status 1"), false},
		{"quoted prompt", execx.Result{Stdout: "Investigate: " + diagnostic}, errors.New("exit status 1"), false},
		{"ordinary failure", execx.Result{Stdout: "Build failed"}, errors.New("exit status 1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
				return tc.result, tc.err
			}))
			_, err := h.runCodexWithHeartbeat(context.Background(), agentruntime.Runtime{Harness: "claude", Command: "claude"}, t.TempDir(), "task", codexRunOptions{}, agentInvocationLogMetadata{})
			if got := errors.Is(err, agentruntime.ErrClaudeAuthRequired); got != tc.wantAuth {
				t.Fatalf("authentication required = %v, want %v; err=%v", got, tc.wantAuth, err)
			}
		})
	}
}
