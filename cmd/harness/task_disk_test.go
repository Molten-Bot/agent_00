package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestTaskWorkspaceLeaseReleasesAndReadmitsAfterRemoteWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	const unit = workspace.MinimumFreeBytes
	var free atomic.Uint64
	free.Store(unit)
	logs := make(chan string, 16)
	logf := func(format string, args ...any) { logs <- fmt.Sprintf(format, args...) }
	scheduler := localruntime.NewAdaptiveScheduler(localruntime.SchedulerConfig{MaxParallel: 1}, logf,
		localruntime.WithSchedulerDiskSpace(func(string) (string, uint64, uint64, error) {
			return "disk", free.Load(), 4 * unit, nil
		}))
	disk := localruntime.TaskDiskRequest{Workspace: workspace.Manager{Root: t.TempDir()}}
	_, release, err := scheduler.AcquireWorkspace(ctx, "retained-task", disk, false)
	if err != nil {
		t.Fatal(err)
	}
	lease := taskWorkspaceLease{
		scheduler: scheduler, requestID: "retained-task", disk: disk,
		handle: &localTaskHandle{running: true}, logf: logf, release: release,
	}
	lease.suspend()

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
			if !strings.Contains(line, "request_id=retained-task") || !strings.Contains(line, "reason=disk_space") {
				continue
			}
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
			return
		case <-ctx.Done():
			t.Fatal("retained task did not resume")
		}
	}
}
