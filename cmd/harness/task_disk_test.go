package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestWorkspaceAdmissionPausesDraftVerificationWatchdog(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	const timeout = 50 * time.Millisecond
	watchdog := localruntime.NewStepWatchdog(timeout, cancel)
	defer watchdog.Close()
	h := app.Harness{
		AcquireWorkspace: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(2 * timeout):
				return nil
			}
		},
	}
	pauseWatchdogDuringAdmission(&h, watchdog)
	watchdog.Advance("pr_verification")
	if err := h.AcquireWorkspace(ctx); err != nil {
		t.Fatalf("draft verification timed out in admission: %v", err)
	}
	select {
	case <-ctx.Done():
		var timeoutErr *localruntime.StepTimeoutError
		if !errors.As(context.Cause(ctx), &timeoutErr) || timeoutErr.Step != "pr_verification" {
			t.Fatalf("unexpected watchdog error: %v", context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("draft verification watchdog did not resume")
	}
}

func TestTaskWorkspaceLeaseCreditsCheckoutGrowthOnReadmission(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "retained")
	history, err := localruntime.OpenTaskDiskHistory("")
	if err != nil {
		t.Fatal(err)
	}
	const estimate = workspace.MinimumFreeBytes + 4096
	if err := history.Record("repo", estimate); err != nil {
		t.Fatal(err)
	}
	disk := localruntime.TaskDiskRequest{
		Workspace: workspace.Manager{Root: root}, ExistingDir: dir,
		RequiredBytes: localTaskDiskRequirement(history, "repo", dir),
	}
	free := uint64(estimate)
	logf := func(string, ...any) {}
	scheduler := localruntime.NewAdaptiveScheduler(localruntime.SchedulerConfig{MaxParallel: 1}, logf,
		localruntime.WithSchedulerDiskSpace(func(string) (string, uint64, uint64, error) {
			return "disk", free, 4 * workspace.MinimumFreeBytes, nil
		}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, release, err := scheduler.AcquireWorkspace(ctx, "retained-task", disk, false)
	if err != nil {
		t.Fatal(err)
	}
	lease := taskWorkspaceLease{
		scheduler: scheduler, requestID: "retained-task", disk: disk,
		history: history, diskKey: "repo", handle: &localTaskHandle{running: true}, logf: logf, release: release,
	}
	defer lease.suspend()
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture"), make([]byte, 64*1024), 0600); err != nil {
		t.Fatal(err)
	}
	lease.suspend()
	free = workspace.MinimumFreeBytes
	if err := lease.acquire(ctx); err != nil {
		t.Fatalf("retained workspace could not readmit after checkout growth: %v", err)
	}
}

func TestTaskWorkspaceLeaseReleasesAndReadmitsAfterRemoteWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	store, record := lifecycleRecord(t)
	issue := issueExecution{store: store, record: record, policy: (localruntime.IssueRecoveryConfig{}).Defaults()}
	h := app.Harness{}
	ctx, finish := issue.configure(ctx, &h)
	defer finish()
	h.OnStep("review")
	broker := web.NewBroker()
	assertState := func(state, status string) {
		t.Helper()
		claim, err := store.Load(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if claim.State != state || claim.Step != "review" {
			t.Fatalf("claim state/step = %q/%q, want %q/review", claim.State, claim.Step, state)
		}
		task, ok := broker.Task(record.RequestID)
		if !ok || task.Status != status || task.ExecutionState != state {
			t.Fatalf("broker task found = %v, status/state = %q/%q, want %q/%q", ok, task.Status, task.ExecutionState, status, state)
		}
	}

	const unit = workspace.MinimumFreeBytes
	var free atomic.Uint64
	free.Store(unit)
	logs := make(chan string, 16)
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		broker.IngestLog(line)
		logs <- line
	}
	scheduler := localruntime.NewAdaptiveScheduler(localruntime.SchedulerConfig{MaxParallel: 1}, logf,
		localruntime.WithSchedulerDiskSpace(func(string) (string, uint64, uint64, error) {
			return "disk", free.Load(), 4 * unit, nil
		}))
	disk := localruntime.TaskDiskRequest{Workspace: workspace.Manager{Root: t.TempDir()}}
	_, release, err := scheduler.AcquireWorkspace(ctx, record.RequestID, disk, false)
	if err != nil {
		t.Fatalf("initial admission: %v (context cause: %v)", err, context.Cause(ctx))
	}
	lease := taskWorkspaceLease{
		scheduler: scheduler, requestID: record.RequestID, disk: disk,
		handle: &localTaskHandle{running: true}, logf: logf, release: release, onState: h.OnTaskState,
	}
	lease.reportState("running")
	lease.yield("waiting_for_codex_session")
	assertState("waiting_for_codex_session", "pending")

	_, releaseOther, err := scheduler.AcquireWorkspace(ctx, "other-task", disk, false)
	if err != nil {
		t.Fatalf("scheduler could not use released reservation: %v", err)
	}
	free.Store(0)
	releaseOther()

	done := make(chan error, 1)
	go func() { done <- lease.acquire(ctx) }()
	for {
		select {
		case line := <-logs:
			if !strings.Contains(line, "request_id="+record.RequestID) || !strings.Contains(line, "reason=disk_space") {
				continue
			}
			assertState("queued", "pending")
			free.Store(unit)
			scheduler.RecheckQueue()
		case err := <-done:
			defer lease.suspend()
			if err != nil {
				t.Fatal(err)
			}
			if free.Load() != unit || lease.release == nil {
				t.Fatal("retained task resumed without passing disk admission")
			}
			assertState("running", "running")
			return
		case <-ctx.Done():
			t.Fatal("retained task did not resume")
		}
	}
}
