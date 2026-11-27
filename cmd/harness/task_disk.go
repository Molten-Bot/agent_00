package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

// Retry dirty writes even when a completed workspace was retained or the queue is idle.
func retryLocalTaskDiskHistory(ctx context.Context, history *localruntime.TaskDiskHistory, interval time.Duration, logf func(string, ...any)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	flush := func() {
		if err := history.Flush(); err != nil {
			logf("scheduler status=warn action=flush_task_disk_history err=%q", err)
		}
	}
	for {
		select {
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}

func openLocalTaskDiskHistory(configPath string) (*localruntime.TaskDiskHistory, error) {
	configPath = filepath.Clean(configPath)
	// Runtime config writes can replace its symlink. Keep the sidecar named
	// after the configured path so those writes don't change its location.
	historyName := "." + filepath.Base(configPath) + ".task-disk-usage.json"
	historyPath := filepath.Join(filepath.Dir(configPath), historyName)
	configInfo, err := os.Stat(configPath)
	if err != nil {
		_, lstatErr := os.Lstat(configPath)
		if !errors.Is(err, os.ErrNotExist) || !errors.Is(lstatErr, os.ErrNotExist) {
			history, _ := localruntime.OpenTaskDiskHistory("")
			return history, fmt.Errorf("inspect runtime config for disk history: %w", err)
		}
	}
	for _, path := range []string{historyPath, localruntime.TaskDiskSamplesPath(historyPath)} {
		historyInfo, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			history, _ := localruntime.OpenTaskDiskHistory("")
			return history, fmt.Errorf("inspect disk history path: %w", err)
		}
		if historyInfo != nil && historyInfo.Mode()&os.ModeSymlink != 0 {
			history, _ := localruntime.OpenTaskDiskHistory("")
			return history, fmt.Errorf("disk history path is a symlink")
		}
		if configInfo != nil && historyInfo != nil && os.SameFile(configInfo, historyInfo) {
			history, _ := localruntime.OpenTaskDiskHistory("")
			return history, fmt.Errorf("disk history path refers to the runtime config file")
		}
	}
	return localruntime.OpenTaskDiskHistory(historyPath)
}

func canRetryTaskWorkspace(manager workspace.Manager) bool {
	// Let admission check space on both roots when retrying allocation.
	manager.CheckSpace = nil
	return len(manager.DiskRoots()) > 1
}

func (w *issueRepairWork) workspaceDir() string {
	return filepath.Join(filepath.Dir(w.queue.claims.Root), "repair-workspaces", w.record.ID)
}

func localTaskDiskRequest(ctx context.Context, history *localruntime.TaskDiskHistory, key string) localruntime.TaskDiskRequest {
	request := localruntime.TaskDiskRequest{Workspace: workspace.NewManager()}
	if repair, ok := ctx.Value(issueRepairContextKey{}).(*issueRepairWork); ok {
		request.ExistingDir = repair.workspaceDir()
	}
	if issue, ok := ctx.Value(issueExecutionContextKey{}).(*issueExecution); ok {
		request.ExistingDir = issue.record.WorkspaceDir
		request.CheckPersistentStorage = workspace.CheckPersistentStorage
	}
	if request.ExistingDir != "" {
		request.Workspace.Root = filepath.Dir(request.ExistingDir)
	}
	request.RequiredBytes = localTaskDiskRequirement(history, key, request.ExistingDir)
	return request
}

func localTaskDiskRequirement(history *localruntime.TaskDiskHistory, key, dir string) func() uint64 {
	var existingBytes uint64
	if dir != "" {
		if size, err := workspace.DiskUsage(dir); err == nil {
			existingBytes = size
		}
	}
	return func() uint64 {
		required := history.RequiredBytes(key)
		if required == 0 {
			return workspace.MinimumFreeBytes
		}
		return required - min(required, existingBytes)
	}
}

func logIssueWorkspaceStorage(path, requestID string, logf func(string, ...any)) {
	device, free, total, err := workspace.DiskSpace(path)
	if err == nil {
		err = workspace.CheckPersistentStorage(path)
	}
	if err != nil {
		logf("workspace_storage status=error request_id=%s path=%q filesystem=%q free_bytes=%d total_bytes=%d err=%q", requestID, path, device, free, total, err)
	} else if free < workspace.MinimumFreeBytes {
		logf("workspace_storage status=warn request_id=%s path=%q filesystem=%q free_bytes=%d total_bytes=%d minimum_free_bytes=%d", requestID, path, device, free, total, workspace.MinimumFreeBytes)
	} else {
		logf("workspace_storage status=ok request_id=%s path=%q filesystem=%q free_bytes=%d total_bytes=%d", requestID, path, device, free, total)
	}
}

func recordLocalTaskDiskUsage(history *localruntime.TaskDiskHistory, key, requestID, runDir string, logf func(string, ...any)) {
	if key == "" || runDir == "" {
		return
	}
	size, err := workspace.DiskUsage(runDir)
	if err == nil {
		err = history.Record(key, size)
	}
	if err != nil {
		logf("scheduler status=warn action=record_task_disk_usage request_id=%s err=%q", requestID, err)
	}
}

type taskWorkspaceLeaseKey struct{}

// The task goroutine owns this lease. A remote wait releases both its disk
// reservation and scheduler slot while keeping the task and checkout intact.
type taskWorkspaceLease struct {
	scheduler *localruntime.AdaptiveScheduler
	requestID string
	disk      localruntime.TaskDiskRequest
	history   *localruntime.TaskDiskHistory
	diskKey   string
	handle    *localTaskHandle
	logf      func(string, ...any)
	timings   *app.CheckTimings
	release   func()
	onState   func(string)
}

func (l *taskWorkspaceLease) suspend() {
	if l.release != nil {
		l.release()
		l.release = nil
	}
}

func (l *taskWorkspaceLease) yield(state string) {
	l.handle.SetRunning(false)
	l.reportState(state)
	l.suspend()
}

func (l *taskWorkspaceLease) reportState(state string) {
	if l.onState != nil {
		l.onState(state)
	}
	l.logf("task request_id=%s task_state=%s", l.requestID, state)
}

func (l *taskWorkspaceLease) acquire(ctx context.Context) error {
	if l.release != nil {
		return nil
	}
	l.logf("task status=queued request_id=%s action=wait_for_workspace", l.requestID)
	l.handle.SetRunning(false)
	l.reportState("queued")
	disk := l.disk
	if l.history != nil {
		disk.RequiredBytes = localTaskDiskRequirement(l.history, l.diskKey, disk.ExistingDir)
	}
	force := false
	for {
		if err := l.handle.WaitUntilRunnable(ctx); err != nil {
			return err
		}
		acquireCtx, cancel := context.WithCancel(ctx)
		l.handle.SetAcquireCancel(cancel)
		force = l.handle.ConsumeForceAcquire() || force
		_, release, err := l.scheduler.AcquireWorkspace(acquireCtx, l.requestID, disk, force)
		l.handle.ClearAcquireCancel(cancel)
		cancel()
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.Canceled) && (force || l.handle.IsPaused() || l.handle.HasForceAcquire()) {
				continue
			}
			return err
		}
		if l.handle.IsPaused() || l.handle.IsStopped() {
			release()
			continue
		}
		l.release = release
		l.handle.SetRunning(true)
		l.logf("task status=admitted request_id=%s action=resume_workspace", l.requestID)
		l.reportState("running")
		return nil
	}
}

func pauseWatchdogDuringAdmission(h *app.Harness, watchdog *localruntime.StepWatchdog) {
	if acquire := h.AcquireWorkspace; acquire != nil {
		h.AcquireWorkspace = func(ctx context.Context) error {
			resume := watchdog.Pause()
			defer resume()
			return acquire(ctx)
		}
	}
	if wait := h.OnCodexSessionWait; wait != nil {
		h.OnCodexSessionWait = func() func() {
			resume := watchdog.Pause()
			finishWait := wait()
			return func() {
				finishWait()
				resume()
			}
		}
	}
}
