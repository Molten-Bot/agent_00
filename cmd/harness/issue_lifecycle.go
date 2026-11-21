package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

type issueExecutionContextKey struct{}
type issueExecution struct {
	store  *localruntime.IssueClaimStore
	record localruntime.IssueClaimRecord
	lock   *os.File
	policy localruntime.IssueRecoveryConfig
}

func prepareIssueExecution(store *localruntime.IssueClaimStore, run config.Config, policy localruntime.IssueRecoveryConfig) (*issueExecution, string, error) {
	if run.GitHubIssue == nil || run.GitHubIssue.Claim == nil {
		return nil, "", nil
	}
	record, err := store.Load(run.GitHubIssue.Claim.ID)
	if err != nil {
		return nil, "", err
	}
	if record.Version != 2 {
		return nil, "", nil
	}
	owner, err := store.Identity()
	if err != nil {
		return nil, "", fmt.Errorf("load issue owner: %w", err)
	}
	if owner != record.InstanceID {
		return nil, "", fmt.Errorf("issue ownership changed; start the issue again to create a new attempt")
	}
	if record.RequestID == "" {
		return nil, "", fmt.Errorf("issue dispatch intent has no request ID")
	}
	if record.Control != "" {
		return nil, "", fmt.Errorf("issue is %s; explicit resume required", record.Control)
	}
	if record.Outcome != "" {
		return nil, record.RequestID, nil
	}
	lock, err := store.TryLock(localruntime.IssueExecutionKey(run.GitHubIssue))
	if err != nil {
		return nil, "", err
	}
	if lock == nil {
		return nil, record.RequestID, nil
	}
	if record.WorkspaceDir == "" {
		record.WorkspaceDir = filepath.Join(filepath.Dir(store.Root), "issue-workspaces", record.ID)
	}
	// A failed clone may leave a partial directory without a repository. Preserve
	// that evidence, but let the next attempt clone into its own durable directory.
	if filepath.Base(record.WorkspaceDir) != record.ID {
		entries, readErr := os.ReadDir(record.WorkspaceDir)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			lock.Close()
			return nil, "", readErr
		}
		hasRepository := false
		for _, entry := range entries {
			if entry.IsDir() {
				if info, err := os.Stat(filepath.Join(record.WorkspaceDir, entry.Name(), ".git")); err == nil && info.IsDir() {
					hasRepository = true
					break
				}
			}
		}
		if !hasRepository {
			record.WorkspaceDir = filepath.Join(filepath.Dir(store.Root), "issue-workspaces", record.ID)
		}
	}
	record.State = "queued"
	record.Error = ""
	record.NextAttemptAt = time.Time{}
	if err := store.Save(record); err != nil {
		lock.Close()
		return nil, "", err
	}
	return &issueExecution{store: store, record: record, lock: lock, policy: policy.Defaults()}, "", nil
}

func persistIssueControl(store *localruntime.IssueClaimStore, requestID, control string) error {
	records, err := store.List()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Version == 2 && record.RequestID == requestID {
			return store.Update(record.ID, func(r *localruntime.IssueClaimRecord) error { r.Control = control; return nil })
		}
	}
	return nil
}

func (e *issueExecution) finish(ctx context.Context, state string, result app.Result) error {
	defer e.lock.Close()
	if state == "" {
		state = "interrupted"
	}
	if result.NoChanges && !resultHasPR(result) && state != "stopped" {
		state = "no_changes"
	}
	if ctx.Err() != nil && state != "stopped" && state != "completed" && state != "no_changes" {
		state = "interrupted"
	}
	return e.store.Update(e.record.ID, func(record *localruntime.IssueClaimRecord) error {
		record.Outcome = state
		record.ExitCode = result.ExitCode
		record.State = "verifying"
		record.StoppedAt = time.Now().UTC()
		record.UnlockPending = record.LabelAdded
		if result.WorkspaceDir != "" {
			record.WorkspaceDir = result.WorkspaceDir
		}
		if result.Err != nil {
			record.Error = result.Err.Error()
		}
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			record.Error = cause.Error()
		}
		record.OutcomeError = record.Error
		if state != "completed" {
			record.Incident = "missing_pr"
		}
		record.NoChangeEvidence = result.NoChangeEvidence
		if state == "interrupted" && errors.Is(context.Cause(ctx), context.Canceled) {
			record.Incident = "service_shutdown"
		}
		return nil
	})
}

func (e *issueExecution) configure(ctx context.Context, h *app.Harness) (context.Context, func()) {
	runCtx, cancel := context.WithCancelCause(ctx)
	watchdog := localruntime.NewStepWatchdog(time.Duration(e.policy.StepTimeoutSeconds)*time.Second, cancel)
	h.Workspace.Root = filepath.Dir(e.record.WorkspaceDir)
	h.Workspace.NewGUID = func() string { return filepath.Base(e.record.WorkspaceDir) }
	h.ResumeIssueWorkspace = true
	startedAgent := false
	h.OnStep = func(step string) {
		if !watchdog.Advance(step) {
			return
		}
		err := e.store.Update(e.record.ID, func(record *localruntime.IssueClaimRecord) error {
			record.State = "running"
			record.Step = step
			record.StepStartedAt = time.Now().UTC()
			if record.StartedAt.IsZero() {
				record.StartedAt = record.StepStartedAt
			}
			if step == "agent" && !startedAgent {
				record.AgentAttempts++
				startedAgent = true
			}
			return nil
		})
		if err != nil {
			cancel(fmt.Errorf("persist workflow progress: %w", err))
		}
	}
	return runCtx, func() { watchdog.Close(); cancel(nil) }
}
