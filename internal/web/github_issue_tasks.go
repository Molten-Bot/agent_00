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
	Number    int      `json:"number"`
	RequestID string   `json:"request_id,omitempty"`
	Status    string   `json:"status"`
	Stage     string   `json:"stage,omitempty"`
	Stages    []string `json:"stages,omitempty"`
	Error     string   `json:"error,omitempty"`
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
		if !live && (latest.Version != 2 || s.Broker.issueTaskClosed(requestID) ||
			(isCompletedTaskStatus(latest.Outcome) && latest.State != "blocked" &&
				latest.State != "retry_wait" && time.Since(latest.UpdatedAt) >= defaultClosedTaskRetention)) {
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
		if latest.Run.Workflow != nil && !latest.Run.Workflow.IsDefaultExecution() {
			task.WorkflowID, task.WorkflowName = latest.Run.Workflow.ID, latest.Run.Workflow.Name
			task.WorkflowTriggered = true
			if run := delivery[latest.Run.Workflow.EventID]; run.RequestID == latest.RequestID {
				switch run.Status {
				case "waiting", "awaiting_review":
					task.IssueState = run.Status
					task.ReviewGate = run.Reason
					task.RetryAt = stringsx.FirstNonEmpty(formatTimeOrEmpty(run.GitHubRetryAt), task.RetryAt)
					task.PRURL = stringsx.FirstNonEmpty(task.PRURL, run.PRURL)
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
		switch {
		case latest.PlanningStatus == issueDone || task.Status == "merged":
			task.Status, task.Error, task.ReviewGate = "merged", "", ""
		case latest.Outcome == "stopped" || task.Status == "stopped":
			task.Status, task.IssueState, task.ReviewGate, task.RetryAt = "stopped", "stopped", "", ""
			task.Error, task.Warning = "task was stopped by operator", ""
		case latest.Control == "paused" && !latest.Fulfilled():
			task.Status, task.IssueState, task.RetryAt = "paused", "paused", ""
		case task.IssueState == "retry_wait" || task.IssueState == localruntime.IssueChecksPending ||
			task.IssueState == "waiting" || task.IssueState == "awaiting_review":
			if isCompletedTaskStatus(task.Status) || !live {
				task.Status = "queued"
			}
			task.Error = stringsx.FirstNonEmpty(latest.Error, task.Error)
		case task.IssueState == "blocked" || task.IssueState == localruntime.IssueStateMergedUnverified:
			task.Status = "error"
			task.Error = stringsx.FirstNonEmpty(latest.Error, latest.RunError, task.Error)
		}
		task.Controls.Stop = s.canStopRetainedIssueTask(latest, task)

		for i, attempt := range attempts {
			stage := attempt.Step
			status := stringsx.FirstNonEmpty(attempt.Outcome, attempt.State, attempt.Phase)
			if i == len(attempts)-1 && live {
				stage = stringsx.FirstNonEmpty(stage, task.Stage)
				status = stringsx.FirstNonEmpty(attempt.Outcome, task.Status, status)
			}
			task.IssueAttempts = append(task.IssueAttempts, issueTaskAttempt{
				Number: attempt.Run.GitHubIssue.Claim.Attempt, RequestID: attempt.RequestID,
				Status: status, Stage: stage, Stages: attempt.Stages, Error: attempt.FailureDetail(),
			})
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

func (b *Broker) issueTaskClosed(requestID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.isClosedTaskLocked(requestID, b.now())
}

func (b *Broker) notifyIssueTaskUpdate() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notifySubscribersLocked()
}
