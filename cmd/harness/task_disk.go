package main

import (
	"context"
	"path/filepath"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

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
