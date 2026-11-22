package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	historyInfo, err := os.Lstat(historyPath)
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
	}
	var existingBytes uint64
	if request.ExistingDir != "" {
		request.Workspace.Root = filepath.Dir(request.ExistingDir)
		if size, err := workspace.DiskUsage(request.ExistingDir); err == nil {
			existingBytes = size
		}
	}
	request.RequiredBytes = func() uint64 {
		required := history.RequiredBytes(key)
		if required == 0 {
			return workspace.MinimumFreeBytes
		}
		return required - min(required, existingBytes)
	}
	return request
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
