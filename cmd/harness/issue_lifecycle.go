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
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

type issueExecutionContextKey struct{}
type issueExecution struct {
	store  *localruntime.IssueClaimStore
	record localruntime.IssueClaimRecord
	lock   *os.File
	policy localruntime.IssueRecoveryConfig
}

func checkLocalIssueOwnership(ctx context.Context, run config.Config, requestID string, task *localTaskHandle, verify func(context.Context, config.Config) error, logf func(string, ...any)) localTaskOutcome {
	if verify == nil {
		return localTaskOutcome{}
	}
	err := verify(ctx, run)
	if err == nil {
		return localTaskOutcome{}
	}
	result := app.Result{ExitCode: app.ExitPreflight, Err: err}
	if task.IsStopped() {
		logf("task status=stopped request_id=%s err=%q", requestID, errTaskStoppedByOperator)
		return localTaskOutcome{State: "stopped", Result: result}
	}
	logf("task status=error request_id=%s err=%q", requestID, err)
	return localTaskOutcome{State: "error", Result: result}
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
	if err := store.CheckOwner(record.InstanceID); err != nil {
		return nil, "", err
	}
	if record.RequestID == "" {
		return nil, "", fmt.Errorf("issue dispatch intent has no request ID")
	}
	lock, err := store.TryLock(localruntime.IssueExecutionKey(run.GitHubIssue))
	if err != nil {
		return nil, "", err
	}
	if lock == nil {
		return nil, record.RequestID, nil
	}
	// The previous worker may have recorded its outcome before releasing the lock.
	record, err = store.Load(record.ID)
	if err != nil {
		lock.Close()
		return nil, "", err
	}
	if record.Control != "" {
		lock.Close()
		return nil, "", fmt.Errorf("issue is %s; explicit resume required", record.Control)
	}
	if record.Outcome != "" {
		lock.Close()
		return nil, record.RequestID, nil
	}
	// A partial clone can have a .git directory and a configured origin.
	// Preserve it as evidence and give the next attempt a fresh directory.
	if record.WorkspaceDir == "" || !record.CheckoutComplete {
		record.WorkspaceDir = filepath.Join(filepath.Dir(store.Root), "issue-workspaces", record.ID)
		record.CheckoutComplete = false
		record.PublicationReady = false
	}
	record.State = "queued"
	record.Error = ""
	record.NextAttemptAt = time.Time{}
	if err := store.BeginExecution(record); err != nil {
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

func (e *issueExecution) finish(ctx context.Context, state string, result app.Result, waitForStorage func() bool) error {
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
	stoppedAt := time.Now().UTC()
	var journalErr error
	holdPersisted := false
	update := func(record *localruntime.IssueClaimRecord) error {
		record.Outcome = state
		record.ExitCode = result.ExitCode
		record.State = "verifying"
		record.StoppedAt = stoppedAt
		record.UnlockPending = record.LabelAdded
		if result.WorkspaceDir != "" {
			record.WorkspaceDir = result.WorkspaceDir
		}
		record.RunError = ""
		if result.Err != nil {
			record.RunError = result.Err.Error()
			record.Error = record.RunError
		}
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			record.Error = cause.Error()
		}
		if state != "completed" {
			record.Incident = "missing_pr"
		}
		record.NoChangeEvidence = result.NoChangeEvidence
		if state == "interrupted" && errors.Is(context.Cause(ctx), context.Canceled) {
			record.Incident = "service_shutdown"
		}
		if journalErr != nil || ((state == "error" || state == "interrupted") &&
			(workspace.IsDiskFull(result.Err) || errors.Is(result.Err, workspace.ErrUnsuitableStorage))) {
			record.Incident = localruntime.IssueIncidentWorkspaceStorage
			record.State = "blocked"
			record.NextAttemptAt = time.Time{}
			if journalErr != nil {
				record.RunError = errors.Join(result.Err, journalErr).Error()
				record.Error = record.RunError
			}
			// This execution can charge at most one agent attempt. Restore the
			// prior count even if a previous write committed before Sync failed.
			record.AgentAttempts = e.record.AgentAttempts
		}
		record.ExecutionAgentAttempts = nil
		return nil
	}
	for {
		err := e.store.Update(e.record.ID, update)
		if err == nil {
			return e.store.ReleaseExecutionReserve(e.record.ID)
		}
		if !workspace.IsDiskFull(err) {
			return err
		}
		if journalErr == nil {
			journalErr = fmt.Errorf("persist issue outcome: %w", err)
		}
		if !holdPersisted {
			if holdErr := e.store.ReleaseExecutionReserve(e.record.ID); holdErr != nil {
				err = errors.Join(err, fmt.Errorf("persist workspace storage retry hold: %w", holdErr))
			} else {
				holdPersisted = true
			}
		}
		if waitForStorage == nil || !waitForStorage() {
			return err
		}
	}
}

func (e *issueExecution) configure(ctx context.Context, h *app.Harness) (context.Context, func()) {
	runCtx, cancel := context.WithCancelCause(ctx)
	watchdog := localruntime.NewStepWatchdog(time.Duration(e.policy.StepTimeoutSeconds)*time.Second, cancel)
	h.Workspace.Root = filepath.Dir(e.record.WorkspaceDir)
	h.Workspace.NewGUID = func() string { return filepath.Base(e.record.WorkspaceDir) }
	h.ResumeIssueWorkspace = true
	h.IssueCheckoutComplete = e.record.CheckoutComplete
	h.IssuePublicationReady = e.record.PublicationReady
	onCheckout := h.OnCheckout
	h.OnCheckout = func() {
		if onCheckout != nil {
			onCheckout()
		}
		if e.record.CheckoutComplete {
			return
		}
		if err := e.store.Update(e.record.ID, func(record *localruntime.IssueClaimRecord) error {
			record.CheckoutComplete = true
			return nil
		}); err != nil {
			cancel(fmt.Errorf("persist completed checkout: %w", err))
		}
	}
	h.SaveIssuePublicationReady = func(ready bool) error {
		return e.store.Update(e.record.ID, func(record *localruntime.IssueClaimRecord) error {
			record.PublicationReady = ready
			return nil
		})
	}
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
