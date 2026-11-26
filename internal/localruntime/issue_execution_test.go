package localruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestIssueRunnerChecksOwnershipBeforeChangingDraftState(t *testing.T) {
	t.Parallel()
	ownerChanged := errors.New("issue ownership changed")
	runner := IssueRunner{Check: func(context.Context) error { return ownerChanged }}
	for _, args := range [][]string{{"pr", "ready", "42"}, {"pr", "ready", "42", "--undo"}} {
		if _, err := runner.Run(t.Context(), execx.Command{Name: "gh", Args: args}); !errors.Is(err, ownerChanged) {
			t.Fatalf("gh %v returned %v, want ownership failure", args, err)
		}
	}
}

func TestIssueWorkerLockSurvivesParentDescriptorClose(t *testing.T) {
	if os.Getenv("ISSUE_LOCK_CHILD") == "1" {
		io.WriteString(os.Stdout, "ready\n")
		io.Copy(io.Discard, os.Stdin)
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix inherited locks")
	}
	t.Parallel()
	store := &IssueClaimStore{Root: t.TempDir()}
	lock, err := store.TryLock("worker")
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestIssueWorkerLockSurvivesParentDescriptorClose$")
	cmd.Env = append(os.Environ(), "ISSUE_LOCK_CHILD=1")
	cmd.ExtraFiles = []*os.File{lock}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	ready := make([]byte, 6)
	if _, err := io.ReadFull(stdout, ready); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if duplicate, err := store.TryLock("worker"); err != nil || duplicate != nil {
		if duplicate != nil {
			duplicate.Close()
		}
		t.Fatalf("surviving subprocess lost lock: %v", err)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	acquired, err := store.TryLock("worker")
	if err != nil || acquired == nil {
		t.Fatalf("exited worker retained lock: %v", err)
	}
	acquired.Close()
}

func TestIssueStepWatchdogFollowsProgressNotTaskAge(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	w := NewStepWatchdog(150*time.Millisecond, cancel)
	defer w.Close()
	for _, step := range []string{"clone", "agent", "push", "pr"} {
		w.Advance(step)
		select {
		case <-ctx.Done():
			t.Fatalf("progressing task timed out: %v", context.Cause(ctx))
		case <-time.After(60 * time.Millisecond):
		}
	}
	// Output/retries at the same workflow step cannot extend the deadline.
	for range 3 {
		w.Advance("pr")
		time.Sleep(40 * time.Millisecond)
	}
	select {
	case <-ctx.Done():
		var timeout *StepTimeoutError
		if cause := context.Cause(ctx); !errors.As(cause, &timeout) || timeout.Step != "pr" || timeout.Timeout != 150*time.Millisecond {
			t.Fatal(cause)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled workflow did not time out")
	}
}

func TestIssueIdentityAndHealthyRecordsSurviveRestart(t *testing.T) {
	t.Parallel()
	store := &IssueClaimStore{Root: t.TempDir()}
	id, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	restarted := &IssueClaimStore{Root: store.Root}
	got, err := restarted.Identity()
	if err != nil || got != id {
		t.Fatalf("identity changed: %s %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(store.Root, "broken.json"), []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("corruption not visible")
	}
	if _, err := store.TryLock("../escape"); err == nil {
		t.Fatal("unsafe lock key accepted")
	}
}
