package app

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// The agent command is the only command the harness runs on a child goroutine,
// and the heartbeat select that waits for it carries no deadline unless
// AgentStageTimeout is configured. A runner that leaves without delivering a
// result - a panic, or the runtime.Goexit that a test helper's t.Fatalf
// performs - therefore stranded the stage forever: the heartbeat kept logging,
// the cancellation arm waited on the same result, and nothing else could fire.
// The only symptom was `go test` killing the whole package at its ten minute
// timeout, which says nothing about the command that actually went wrong. A
// dead runner has to fail the run with a reason instead of hanging it.
func TestRunCodexWithHeartbeatReportsRunnerThatDeliversNoResult(t *testing.T) {
	t.Parallel()

	h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		runtime.Goexit()
		return execx.Result{}, nil
	}))

	targetDir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := h.runCodexWithHeartbeat(
			context.Background(),
			agentruntime.Runtime{Harness: "claude", Command: "claude"},
			targetDir,
			"task",
			codexRunOptions{},
			agentInvocationLogMetadata{},
		)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runCodexWithHeartbeat() returned no error for a runner that delivered no result")
		}
		if !strings.Contains(err.Error(), "exited without returning a result") {
			t.Fatalf("err = %v, want the missing agent result reported", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runCodexWithHeartbeat() hung on a runner that delivered no result")
	}
}
