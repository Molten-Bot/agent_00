package web

import (
	"crypto/sha256"
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
	if policy != nil {
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
		slices.SortFunc(attempts, func(a, b localruntime.IssueClaimRecord) int {
			if n := a.Run.GitHubIssue.Claim.Attempt - b.Run.GitHubIssue.Claim.Attempt; n != 0 {
				return n
			}
			return a.UpdatedAt.Compare(b.UpdatedAt)
		})
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
		}
		task.PRURL = stringsx.FirstNonEmpty(latest.PRURL, task.PRURL)
		if latest.Run.Workflow != nil {
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
		case latest.PlanningStatus == issueDone:
			task.Status, task.Error, task.ReviewGate = "merged", "", ""
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
