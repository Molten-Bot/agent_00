package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

const (
	issueTodo       = "Todo"
	issueInProgress = "In progress"
	issueInReview   = "In review"
	issueDone       = "Done"
	issueAPIVersion = "X-GitHub-Api-Version: 2026-03-10"
)

func (s Server) setIssuePlanningStatus(ctx context.Context, issue *config.GitHubIssueConfig, status string) error {
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{
		"api", "--method", "PATCH", issueAPIPath(issue), "-H", issueAPIVersion, "-f", "status=" + status,
	}})
	if err != nil {
		return fmt.Errorf("move issue to %s: %w", status, err)
	}
	current, err := decodeIssuePlanningState(result.Stdout)
	if err != nil {
		return err
	}
	if !strings.EqualFold(current.Status.Name, status) {
		return fmt.Errorf("GitHub did not confirm issue status %s", status)
	}
	return nil
}

func (s Server) issuePlanningState(ctx context.Context, issue *config.GitHubIssueConfig) (issuePlanningState, error) {
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", issueAPIPath(issue), "-H", issueAPIVersion}})
	if err != nil {
		return issuePlanningState{}, err
	}
	return decodeIssuePlanningState(result.Stdout)
}

func issueAPIPath(issue *config.GitHubIssueConfig) string {
	return fmt.Sprintf("repos/%s/issues/%d", issue.Repository, issue.Number)
}

type issuePlanningState struct {
	State  string
	Status struct{ Name string }
}

func decodeIssuePlanningState(raw string) (issuePlanningState, error) {
	var state issuePlanningState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return state, fmt.Errorf("decode GitHub issue planning status: %w", err)
	}
	return state, nil
}

func (s Server) prepareIssuePlanning(ctx context.Context, issue *config.GitHubIssueConfig, target issueClaimTarget, manual bool) (string, error) {
	if !target.PlanningEnabled {
		return "", nil
	}
	switch {
	case target.PlanningClosed:
		return "", fmt.Errorf("closed issues cannot be started")
	case strings.EqualFold(target.PlanningStatus, issueTodo):
		return issueTodo, nil
	case manual && (target.PlanningStatus == "" || strings.EqualFold(target.PlanningStatus, "Backlog")):
		return issueTodo, s.setIssuePlanningStatus(ctx, issue, issueTodo)
	default:
		return "", fmt.Errorf("issue is ineligible: planning status must be Todo")
	}
}

// Keep planning status separate from claim fulfillment, which proves ownership
// of a published PR and can precede delivery.
func (s Server) syncIssuePlanning(ctx context.Context, record localruntime.IssueClaimRecord) error {
	if record.PlanningStatus == "" || record.PlanningStatus == issueDone || record.PRURL == "" || !record.BranchReserved {
		return nil
	}
	issue := record.Run.GitHubIssue
	canComplete := record.Outcome == "completed" && record.PRVerification == "" && issue.LinkMode == config.GitHubIssueLinkClose
	if record.PlanningStatus == issueInReview && !canComplete {
		return nil
	}
	allowed, err := s.RepositoryAllowed(ctx, issue.Repository)
	if err != nil || !allowed {
		return err
	}
	status := issueInReview
	if canComplete {
		pr, err := s.findIssuePR(ctx, issue)
		if err != nil {
			return err
		}
		if pr.Merged && strings.EqualFold(pr.URL, record.PRURL) && pr.BaseBranch == issue.Claim.BaseBranch &&
			(!record.ReviewRequired || (record.ReviewHead != "" && record.ReviewHead == pr.HeadSHA)) {
			status = issueDone
		}
	}
	if status == record.PlanningStatus {
		return nil
	}
	if err := s.IssueClaims.CheckOwner(record.InstanceID); err != nil {
		if errors.Is(err, localruntime.ErrIssueOwnershipChanged) {
			return nil
		}
		return err
	}
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err != nil || remote.Claim.ID != record.ID || remote.InstanceID != record.InstanceID {
		return err
	}
	current, err := s.issuePlanningState(ctx, issue)
	if err != nil {
		return err
	}
	if current.State != "open" && status != issueDone && current.Status.Name != status {
		return nil
	}
	if current.Status.Name != status {
		if err := s.setIssuePlanningStatus(ctx, issue, status); err != nil {
			return err
		}
	}
	if err := s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		current.PlanningStatus = status
		return nil
	}); err != nil {
		return err
	}
	s.Broker.notifyIssueTaskUpdate()
	return nil
}
