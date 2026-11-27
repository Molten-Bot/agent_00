package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/stringsx"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

type issueTaskAttempt struct {
	Number       int      `json:"number"`
	RequestID    string   `json:"request_id,omitempty"`
	Status       string   `json:"status"`
	Stage        string   `json:"stage,omitempty"`
	Stages       []string `json:"stages,omitempty"`
	Error        string   `json:"error,omitempty"`
	Detail       string   `json:"detail,omitempty"`
	ReviewDetail string   `json:"review_detail,omitempty"`
}

// RecordMonitoredIssueMerge persists the receipt before the broker retires the task.
func (s Server) RecordMonitoredIssueMerge(ctx context.Context, task Task, title string, mergedAt time.Time) (bool, error) {
	if s.IssueClaims == nil {
		return false, nil
	}
	records, listErr := s.IssueClaims.List()
	var latest localruntime.IssueClaimRecord
	for _, record := range records {
		if record.RequestID == task.RequestID &&
			(latest.ID == "" || record.Run.GitHubIssue.Claim.Attempt > latest.Run.GitHubIssue.Claim.Attempt) {
			latest = record
		}
	}
	if latest.ID == "" {
		if listErr != nil && (task.GitHubIssueURL != "" || task.Source == "github_issues") {
			return true, listErr
		}
		return false, nil
	}
	pr := issuePullRequest{URL: task.PRURL, Title: title, MergedAt: mergedAt}
	if err := s.saveMergedIssuePR(latest, pr, "", false); err != nil {
		return true, err
	}
	s.Broker.notifyIssueTaskUpdate()
	return true, s.reconcileIssueRecord(ctx, latest)
}

// RecordIssueMerge persists the linked PR merge and workflow gate on the matching attempt.
func (s *Server) RecordIssueMerge(run workflow.Run, pr workflow.Item, mergedByApp bool) error {
	if s.IssueClaims == nil {
		return nil
	}
	records, listErr := s.IssueClaims.List()
	var latest localruntime.IssueClaimRecord
	for _, record := range records {
		if record.Run.Workflow == nil || record.Run.Workflow.EventID != run.ID ||
			(run.RequestID != "" && record.RequestID != run.RequestID) {
			continue
		}
		if latest.ID == "" || record.Run.GitHubIssue.Claim.Attempt > latest.Run.GitHubIssue.Claim.Attempt ||
			(record.Run.GitHubIssue.Claim.Attempt == latest.Run.GitHubIssue.Claim.Attempt && record.UpdatedAt.After(latest.UpdatedAt)) {
			latest = record
		}
	}
	if latest.ID == "" {
		return listErr
	}
	if known := s.knownIssuePR(latest); known != "" && !strings.EqualFold(known, pr.URL) {
		return nil
	}
	if latest.PRMerged() && (run.Reason == "" || latest.ReviewDetail == run.Reason) &&
		(pr.MergedAt.IsZero() || latest.PRMergedAt.Equal(pr.MergedAt)) && (!mergedByApp || latest.PRMergedByApp) {
		return nil
	}
	return s.saveMergedIssuePR(latest, issuePullRequest{URL: pr.URL, Title: pr.Title, MergedAt: pr.MergedAt}, run.Reason, mergedByApp)
}

func issueTaskID(issue *config.GitHubIssueConfig, policy *config.WorkflowRunConfig) string {
	if issue == nil || issue.Claim == nil {
		return ""
	}
	key := localruntime.IssueExecutionKey(issue)
	if policy != nil && !policy.IsDefaultExecution() {
		key += "/" + policy.ID
	}
	return fmt.Sprintf("github-issue-%x", sha256.Sum256([]byte(key)))
}

// The claim journal retains predecessor attempts after their workers and logs
// expire. Request IDs still address worker controls and assets.
func (s Server) withIssueTasks(snapshot Snapshot) Snapshot {
	if s.IssueClaims == nil {
		return snapshot
	}
	records, _ := s.IssueClaims.List()
	return s.withIssueTaskRecords(snapshot, records)
}

func (s Server) withIssueTaskRecords(snapshot Snapshot, records []localruntime.IssueClaimRecord) Snapshot {
	if len(records) == 0 {
		return snapshot
	}

	byRequest := make(map[string]Task, len(snapshot.Tasks))
	for _, task := range snapshot.Tasks {
		byRequest[task.RequestID] = task
	}
	groups := make(map[string][]localruntime.IssueClaimRecord)
	claimed := make(map[string]bool)
	for _, record := range records {
		if record.RequestID == "" && !record.RemoteRecorded {
			continue
		}
		id := issueTaskID(record.Run.GitHubIssue, record.Run.Workflow)
		groups[id] = append(groups[id], record)
		claimed[record.RequestID] = true
	}

	delivery := map[string]workflow.Run{}
	if s.Workflows != nil && len(groups) > 0 {
		runs, _ := s.Workflows.Runs()
		for _, run := range runs {
			delivery[run.ID] = run
		}
	}
	tasks := slices.DeleteFunc(snapshot.Tasks, func(task Task) bool {
		return claimed[task.RequestID]
	})
	for id, attempts := range groups {
		slices.SortFunc(attempts, compareIssueTaskAttempts)
		latest := attempts[len(attempts)-1]
		requestID := stringsx.FirstNonEmpty(latest.RequestID, "issue-"+latest.ID)
		task, live := byRequest[requestID]
		task.CanRerun = task.CanRerun && isCompletedTaskStatus(task.Status)
		if !live && s.issueTaskHistoryHidden(latest, requestID) {
			continue
		}
		issue := latest.Run.GitHubIssue
		task.TaskID = id
		task.RequestID = requestID
		task.IssueTitle = stringsx.FirstNonEmpty(latest.Run.GitHubIssueTitle(), task.IssueTitle)
		task.GitHubIssueURL = stringsx.FirstNonEmpty(issue.URL, task.GitHubIssueURL)
		task.Repo = stringsx.FirstNonEmpty(task.Repo, latest.Run.RepoURL)
		task.Source = stringsx.FirstNonEmpty(task.Source, "github_issues")
		task.IssueState = latest.State
		if latest.PRRecoveryBlocker != "" {
			task.IssueState = "blocked"
			task.IssuePRRecoveryBlocker = latest.PRRecoveryBlocker
			task.CanRerun = false
		} else if latest.State == "blocked" && requiresIssueBranchRecovery(latest) {
			task.CanRerun = false
		}
		task.IssuePlanningStatus = latest.PlanningStatus
		task.RetryAt = formatTimeOrEmpty(latest.NextAttemptAt)
		if latest.Fulfilled() && (latest.Outcome == "error" || latest.Outcome == "interrupted") {
			task.IssueState = "blocked"
		}
		if !live {
			task.HistoryOnly = true
			task.Error = latest.FailureDetail()
			task.ReviewPolicy = taskReviewPolicy(latest.Run.Workflow, latest.Run.Review != nil || latest.Run.LibraryTaskName == "code-review")
		}
		task.PRURL = stringsx.FirstNonEmpty(latest.PRURL, task.PRURL)
		merged := latest.PRMerged() || latest.PlanningStatus == issueDone || task.Status == "merged"
		if latest.Run.Workflow != nil && !latest.Run.Workflow.IsDefaultExecution() {
			task.WorkflowID, task.WorkflowName = latest.Run.Workflow.ID, latest.Run.Workflow.Name
			task.WorkflowTriggered = true
			if run := delivery[latest.Run.Workflow.EventID]; run.ID != "" && run.RequestID == latest.RequestID &&
				(latest.PRURL == "" || strings.EqualFold(latest.PRURL, run.LinkedPRURL())) {
				knownPR := stringsx.FirstNonEmpty(task.PRURL, latest.Run.Workflow.ResumePRURL)
				if prURL := run.LinkedPRURL(); run.PRMergeConfirmed() && prURL != "" &&
					(knownPR == "" || strings.EqualFold(knownPR, prURL)) {
					merged = true
					task.PRURL = stringsx.FirstNonEmpty(task.PRURL, prURL)
					if run.Status != "merged" {
						latest.ReviewDetail = stringsx.FirstNonEmpty(latest.ReviewDetail, run.Reason)
					}
				}
				switch run.Status {
				case "waiting", "awaiting_review":
					switch task.IssueState {
					case "", "verifying", "fulfilled", "review_pending":
						task.IssueState = run.Status
						task.ReviewGate = run.Reason
						task.RetryAt = stringsx.FirstNonEmpty(formatTimeOrEmpty(run.GitHubRetryAt), task.RetryAt)
						task.PRURL = stringsx.FirstNonEmpty(task.PRURL, run.PRURL)
					}
				}
			}
		}
		if !live {
			switch {
			case isCompletedTaskStatus(latest.Outcome):
				task.Status = latest.Outcome
			case latest.Outcome == "interrupted":
				task.Status = "error"
			case latest.State == "running":
				task.Status = "running"
			default:
				task.Status = "queued"
			}
			task.Stage = latest.Step
			task.StartedAt = formatTimeOrEmpty(latest.StartedAt)
		}
		if latest.UpdatedAt.After(parseTimeOrZero(task.UpdatedAt)) {
			task.UpdatedAt = formatTimeOrEmpty(latest.UpdatedAt)
		}
		if (task.IssueState == "" || task.IssueState == "verifying" || task.IssueState == "fulfilled") &&
			latest.Outcome == workflow.OutcomeReviewPending {
			task.IssueState = workflow.OutcomeReviewPending
		}
		switch {
		case latest.PRMerged() || latest.PlanningStatus == issueDone || task.Status == "merged":
			task.Status, task.Error, task.ReviewGate = "merged", "", ""
		case latest.Outcome == "stopped" || task.Status == "stopped":
			task.Status, task.IssueState, task.ReviewGate, task.RetryAt = "stopped", "stopped", "", ""
			task.Error, task.Warning = "task was stopped by operator", ""
		case latest.Control == "paused" && !latest.Fulfilled():
			task.Status, task.IssueState, task.RetryAt = "paused", "paused", ""
		case task.IssueState == "retry_wait" || task.IssueState == localruntime.IssueChecksPending ||
			task.IssueState == workflow.OutcomeReviewPending ||
			task.IssueState == "waiting" || task.IssueState == "awaiting_review":
			if isCompletedTaskStatus(task.Status) || !live {
				task.Status = "queued"
			}
			task.Error = stringsx.FirstNonEmpty(latest.Error, task.Error)
		case task.IssueState == "blocked":
			task.Status = "error"
			task.Error = stringsx.FirstNonEmpty(latest.PRRecoveryBlocker, latest.Error, latest.RunError, task.Error)
		}
		task.Controls.Stop = s.canStopRetainedIssueTask(latest, task)
		if !live && isCompletedTaskStatus(task.Status) {
			task.DeleteScope = s.Broker.completedAttemptGenerations(requestID)
			if task.DeleteScope[requestID] == "" {
				// The claim ID is also the broker generation assigned at dispatch.
				task.DeleteScope[requestID] = issue.Claim.ID
			}
		}

		for i, attempt := range attempts {
			stage := attempt.Step
			status := stringsx.FirstNonEmpty(attempt.Outcome, attempt.State, attempt.Phase)
			if i == len(attempts)-1 && live {
				stage = stringsx.FirstNonEmpty(stage, task.Stage)
				status = stringsx.FirstNonEmpty(attempt.Outcome, task.Status, status)
			}
			if i == len(attempts)-1 {
				attempt.ReviewDetail = latest.ReviewDetail
			}
			task.IssueAttempts = append(task.IssueAttempts, issueTaskAttempt{
				Number: attempt.Run.GitHubIssue.Claim.Attempt, RequestID: attempt.RequestID,
				Status: status, Stage: stage, Stages: attempt.Stages, Error: attempt.FailureDetail(), Detail: attempt.Error,
				ReviewDetail: attempt.ReviewDetail,
			})
		}
		if merged {
			task.Status, task.IssueState, task.RetryAt = "merged", localruntime.IssueStateMerged, ""
			task.Stage, task.StageStatus, task.ExitCode = "finalize", "merged", 0
			task.CanRerun, task.Controls = false, TaskControls{}
			task.Error, task.ReviewGate, task.Warning = "", "", ""
			task.IssuePRRecoveryBlocker = ""
			if task.PRTitleURL != "" && !strings.EqualFold(task.PRTitleURL, task.PRURL) {
				task.PRTitle, task.PRTitleURL, task.PRTitleUpdatedAt = "", "", ""
			}
		}
		if !live && isCompletedTaskStatus(task.Status) {
			task.DeleteScope = s.Broker.completedAttemptGenerations(requestID)
			if task.DeleteScope[requestID] == "" {
				// The claim ID is also the broker generation assigned at dispatch.
				task.DeleteScope[requestID] = issue.Claim.ID
			}
		}
		for i := len(attempts) - 1; task.PRURL == "" && i >= 0; i-- {
			task.PRURL = stringsx.FirstNonEmpty(attempts[i].PRURL, byRequest[attempts[i].RequestID].PRURL)
		}
		tasks = append(tasks, task)
	}
	slices.SortFunc(tasks, func(a, b Task) int {
		if cmp := strings.Compare(b.UpdatedAt, a.UpdatedAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(b.RequestID, a.RequestID)
	})
	snapshot.Tasks = tasks
	return snapshot
}

func (s Server) canStopRetainedIssueTask(record localruntime.IssueClaimRecord, task Task) bool {
	if record.Version != 2 || s.IssueRecovery == nil || isCompletedTaskStatus(task.Status) {
		return false
	}
	switch {
	case record.Control == "paused", record.Outcome == localruntime.IssueChecksPending,
		record.State == "retry_wait", task.IssueState == "waiting", task.IssueState == "awaiting_review":
		return true
	}
	return record.Outcome == "" && (isPendingTaskStatus(task.Status) || isPausedTaskStatus(task.Status))
}

func compareIssueTaskAttempts(a, b localruntime.IssueClaimRecord) int {
	if n := a.Run.GitHubIssue.Claim.Attempt - b.Run.GitHubIssue.Claim.Attempt; n != 0 {
		return n
	}
	return a.UpdatedAt.Compare(b.UpdatedAt)
}

func (s Server) stopTask(ctx context.Context, requestID string) error {
	if s.StopTask != nil {
		if err := s.StopTask(ctx, requestID); !errors.Is(err, ErrTaskNotFound) {
			return err
		}
	}
	if s.IssueClaims == nil || s.IssueRecovery == nil || s.issueAutomation == nil {
		return ErrTaskNotFound
	}
	persist := func() error { return s.stopRetainedIssueTask(ctx, requestID) }
	if s.Workflows != nil {
		return s.Workflows.StopIssueTask(persist)
	}
	return persist()
}

func (s Server) stopRetainedIssueTask(ctx context.Context, requestID string) error {
	s.issueAutomation.admission.Lock()
	defer s.issueAutomation.admission.Unlock()

	// Admission can attach a worker while the stop waits for the issue lock.
	if s.StopTask != nil {
		if err := s.StopTask(ctx, requestID); !errors.Is(err, ErrTaskNotFound) {
			return err
		}
	}
	records, err := s.IssueClaims.List()
	if err != nil {
		return err
	}
	snapshot := s.withIssueTaskRecords(Snapshot{Tasks: s.Broker.TaskList()}, records)
	for _, task := range snapshot.Tasks {
		if task.RequestID != requestID || task.TaskID == "" {
			continue
		}
		if task.Status == "stopped" {
			return fmt.Errorf("task is already stopped")
		}
		if !task.Controls.Stop {
			return fmt.Errorf("task stop is unavailable for this task")
		}
		var latest localruntime.IssueClaimRecord
		for _, record := range records {
			if (record.RequestID != "" || record.RemoteRecorded) &&
				issueTaskID(record.Run.GitHubIssue, record.Run.Workflow) == task.TaskID &&
				(latest.ID == "" || compareIssueTaskAttempts(record, latest) > 0) {
				latest = record
			}
		}
		if latest.ID != "" && stringsx.FirstNonEmpty(latest.RequestID, "issue-"+latest.ID) == requestID {
			return s.persistRetainedIssueStop(latest)
		}
	}
	return ErrTaskNotFound
}

func (s Server) persistRetainedIssueStop(record localruntime.IssueClaimRecord) error {
	lock, err := s.IssueClaims.TryControlLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("task is changing state; try again")
	}
	defer lock.Close()

	if err := s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		if !current.UpdatedAt.Equal(record.UpdatedAt) {
			return fmt.Errorf("task is changing state; try again")
		}
		current.Control, current.Outcome = "stopped", "stopped"
		current.Error = "task was stopped by operator"
		current.StoppedAt = time.Now().UTC()
		current.NextAttemptAt = time.Time{}
		current.UnlockPending = current.UnlockPending || current.LabelAdded
		return nil
	}); err != nil {
		return err
	}
	s.Broker.recordRetainedIssueStop(stringsx.FirstNonEmpty(record.RequestID, "issue-"+record.ID), record.PRURL)
	s.WakeIssueRecovery()
	return nil
}

func (s Server) issueTaskHistoryHidden(record localruntime.IssueClaimRecord, requestID string) bool {
	return record.Version != 2 || s.Broker.issueTaskClosed(requestID, record.ID) ||
		((record.PRMerged() || isCompletedTaskStatus(record.Outcome)) && record.State != "blocked" &&
			record.State != "retry_wait" && s.Broker.now().Sub(record.UpdatedAt) >= defaultClosedTaskRetention)
}

func (s Server) forgetRestoredIssueTask(requestID string, generations map[string]string) (bool, error) {
	generation := generations[requestID]
	if generation == "" || s.IssueClaims == nil {
		return false, ErrTaskNotFound
	}
	// Use the same projection as status, including its workflow and retry state.
	snapshot := s.withIssueTasks(Snapshot{Tasks: s.Broker.TaskList()})
	for _, task := range snapshot.Tasks {
		if task.RequestID == requestID && task.HistoryOnly && task.DeleteScope[requestID] == generation {
			return s.Broker.forgetRestoredIssueTask(task, generations)
		}
	}
	return false, ErrTaskNotFound
}

func (b *Broker) forgetRestoredIssueTask(task Task, generations map[string]string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := taskResult{task.RequestID, generations[task.RequestID]}
	// A worker may have registered this request since status was projected.
	if b.tasks[task.RequestID] != nil || b.attemptRoots[task.RequestID] != "" ||
		!b.closedIssueTasks[result].IsZero() {
		return false, ErrTaskNotFound
	}
	b.forgetTaskAttemptsLocked(task.TaskID, generations)
	b.closedIssueTasks[result] = b.now().UTC()
	// A wider overlapping retry can still remove matching siblings.
	if len(b.attempts[task.TaskID]) > 0 {
		b.forgottenAttempts[result] = task.TaskID
	}
	b.notifySubscribersLocked()
	return true, nil
}

func (b *Broker) issueTaskClosed(requestID, generation string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	closedAt := b.closedIssueTasks[taskResult{requestID, generation}]
	return (!closedAt.IsZero() && now.Sub(closedAt) < defaultClosedTaskRetention) ||
		b.isClosedTaskLocked(requestID, now)
}

func (b *Broker) notifyIssueTaskUpdate() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notifySubscribersLocked()
}
