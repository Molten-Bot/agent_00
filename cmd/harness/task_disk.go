package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func openLocalTaskDiskHistory(configPath string) (*localruntime.TaskDiskHistory, error) {
	configPath = filepath.Clean(configPath)
	// Runtime config writes can replace its symlink. Keep the sidecar named
	// after the configured path so those writes don't change its location.
	historyName := "." + filepath.Base(configPath) + ".task-disk-usage.json"
	historyPath := filepath.Join(filepath.Dir(configPath), historyName)
	resolved, err := filepath.EvalSymlinks(configPath)
	if err == nil {
		if dir, dirErr := filepath.EvalSymlinks(filepath.Dir(configPath)); dirErr == nil && resolved == filepath.Join(dir, historyName) {
			history, _ := localruntime.OpenTaskDiskHistory("")
			return history, fmt.Errorf("disk history path is the runtime config symlink target")
		}
	} else if _, statErr := os.Lstat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		history, _ := localruntime.OpenTaskDiskHistory("")
		return history, fmt.Errorf("resolve runtime config path for disk history: %w", err)
	}
	if info, err := os.Lstat(historyPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		history, _ := localruntime.OpenTaskDiskHistory("")
		return history, fmt.Errorf("disk history path is a symlink")
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
