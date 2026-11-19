package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

const githubIssueAutoStartPollInterval = time.Minute
const githubIssueCommandTimeout = 30 * time.Second

func (s Server) runIssueGH(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	if s.GitHubIssueRunner == nil {
		return execx.Result{}, fmt.Errorf("GitHub issue runner is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, githubIssueCommandTimeout)
	defer cancel()
	return s.GitHubIssueRunner.Run(ctx, cmd)
}

// Reconcile current eligibility, including backlog. Observing an issue never
// consumes it; only a durable, remotely exclusive claim permits submission.
func (s Server) runGitHubIssueAutoStart(ctx context.Context) {
	if s.GitHubIssueRunner == nil || s.SubmitLocalPrompt == nil || s.ReviewSettingsStatus == nil {
		return
	}
	ticker := time.NewTicker(githubIssueAutoStartPollInterval)
	defer ticker.Stop()
	for {
		s.pollGitHubIssueAutoStart(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.issueAutoStartWake:
		}
	}
}

func (s Server) wakeGitHubIssueAutoStart() {
	if s.issueAutoStartWake == nil {
		return
	}
	select {
	case s.issueAutoStartWake <- time.Now():
	default:
	}
}

func (s Server) pollGitHubIssueAutoStart(ctx context.Context) {
	status := issueAutomationStatus{LastAttempt: time.Now().UTC(), Issues: []issueAutomationDecision{}}
	if s.issueAutomation == nil {
		return
	}
	s.issueAutomation.mu.Lock()
	status.LastSuccess = s.issueAutomation.status.LastSuccess
	s.issueAutomation.status.LastAttempt = status.LastAttempt
	s.issueAutomation.mu.Unlock()
	defer func() {
		if status.Error == "" && status.Enabled {
			status.LastSuccess = time.Now().UTC()
		}
		s.issueAutomation.mu.Lock()
		s.issueAutomation.status = status
		s.issueAutomation.mu.Unlock()
		if status.Error != "" {
			s.logf("localruntime.ui status=warn event=github_issue_auto_start_scan err=%q", status.Error)
		}
	}()
	settings, err := s.ReviewSettingsStatus(ctx)
	if err != nil {
		status.Error = "load automation settings: " + err.Error()
		return
	}
	status.Enabled = settings.GitHubIssuesEnabled && settings.GitHubIssueAutoStartAssigned
	if !status.Enabled {
		return
	}
	status.Account, err = s.issueGitHubLogin(ctx)
	if err != nil {
		status.Error = err.Error()
		return
	}
	if _, err := s.IssueClaims.List(); err != nil {
		status.Error = "read issue claims: " + err.Error()
		return
	}
	// Refresh the shared repository cache periodically, including repositories
	// newly accessible to this account. Failed refreshes remain visible.
	s.issueAutomation.mu.Lock()
	refresh := s.issueAutomation.reposRefreshed.IsZero() || time.Since(s.issueAutomation.reposRefreshed) >= 5*time.Minute
	s.issueAutomation.mu.Unlock()
	var repos []GitHubRepo
	if refresh {
		repos, err = s.refreshGitHubRepos(ctx)
	} else {
		repos, err = s.resolveGitHubRepos(ctx)
	}
	if err != nil {
		status.Error = "resolve repositories: " + err.Error()
		return
	}
	if refresh {
		s.issueAutomation.mu.Lock()
		s.issueAutomation.reposRefreshed = time.Now()
		s.issueAutomation.mu.Unlock()
	}
	repos = filterDisabledGitHubRepos(repos, settings.DisabledRepositories)
	for _, repo := range repos {
		if ctx.Err() != nil {
			status.Error = ctx.Err().Error()
			return
		}
		fullName, ok := validGitHubRepository(repo.FullName)
		if !ok {
			continue
		}
		status.Repositories = append(status.Repositories, fullName)
		issues, err := s.listGitHubIssuesForAutoStart(ctx, fullName)
		if err != nil {
			status.addError(fmt.Errorf("list %s: %w", fullName, err))
			continue
		}
		status.Scanned += len(issues)
		candidates := []githubIssueListEntry{}
		for _, entry := range issues {
			reason := issueEligibilityReason(entry.GitHubIssue, settings, status.Account)
			if reason != "" {
				status.Skipped++
				status.Issues = append(status.Issues, issueAutomationDecision{Repository: fullName, Number: entry.Number, Reason: reason})
				continue
			}
			candidates = append(candidates, entry)
		}
		linked, err := s.githubIssuesWithLinkedBranches(ctx, candidates)
		if err != nil {
			status.addError(err)
			continue
		}
		for _, entry := range candidates {
			decision := issueAutomationDecision{Repository: fullName, Number: entry.Number}
			if linked[entry.ID] {
				decision.Reason = "associated branch or closing PR"
				status.Skipped++
			} else {
				run := issueRunConfig(repo, entry.GitHubIssue, settings.GitHubIssueLinkMode)
				var err error
				decision.RequestID, err = s.claimAndSubmitIssue(ctx, run, settings, status.Account, s.SubmitLocalPrompt)
				if err != nil {
					decision.Reason = "submission blocked"
					decision.Error = err.Error()
					status.addError(err)
				} else {
					decision.Reason = "submitted"
					status.Submitted++
				}
			}
			status.Issues = append(status.Issues, decision)
		}
	}
}

func (s Server) issueGitHubLogin(ctx context.Context) (string, error) {
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "user"}})
	if err != nil {
		return "", fmt.Errorf("resolve GitHub account: %w", err)
	}
	var user GitHubIssueUser
	if err := json.Unmarshal([]byte(result.Stdout), &user); err != nil {
		return "", err
	}
	if strings.TrimSpace(user.Login) == "" {
		return "", fmt.Errorf("GitHub account has no login")
	}
	return user.Login, nil
}

func issueEligibilityReason(issue GitHubIssue, settings ReviewSettingsState, login string) string {
	if !strings.EqualFold(issue.State, "open") {
		return "closed issue"
	}
	if settings.GitHubIssueAutoStartOnlyAssigned {
		assigned := false
		for _, user := range issue.Assignees {
			if strings.EqualFold(user.Login, login) {
				assigned = true
			}
		}
		if !assigned {
			return "wrong assignee"
		}
	}
	if !githubIssueHasAnyLabel(issue, settings.GitHubIssueAutoStartLabels) {
		return "missing trigger label"
	}
	if githubIssueHasAnyLabel(issue, []string{issueExclusionLabel(settings)}) {
		return "claimed elsewhere or excluded"
	}
	return ""
}

func issueRunConfig(repo GitHubRepo, issue GitHubIssue, mode string) config.Config {
	repoURL := repo.HTMLURL
	if repoURL == "" {
		repoURL = "https://github.com/" + repo.FullName
	}
	if mode != config.GitHubIssueLinkClose {
		mode = config.GitHubIssueLinkReference
	}
	run := config.Config{RepoURL: repoURL, BaseBranch: repo.DefaultBranch, GitHubIssue: &config.GitHubIssueConfig{Repository: repo.FullName, Number: issue.Number, LinkMode: mode}}
	setGitHubIssuePrompt(&run, issue)
	return run
}

// REST pagination has no search-result or arbitrary 1,000-issue ceiling.
func (s Server) listGitHubIssuesForAutoStart(ctx context.Context, repository string) ([]githubIssueListEntry, error) {
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "--paginate", "repos/" + repository + "/issues?state=open&per_page=100&sort=created&direction=asc"}})
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	var issues []githubIssueListEntry
	for {
		var page []struct {
			Number      int                `json:"number"`
			ID          string             `json:"node_id"`
			Title       string             `json:"title"`
			State       string             `json:"state"`
			URL         string             `json:"html_url"`
			Author      GitHubIssueUser    `json:"user"`
			Labels      []GitHubIssueLabel `json:"labels"`
			Assignees   []GitHubIssueUser  `json:"assignees"`
			PullRequest json.RawMessage    `json:"pull_request"`
		}
		if err := decoder.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode issue page: %w", err)
		}
		for _, entry := range page {
			if len(entry.PullRequest) > 0 && string(entry.PullRequest) != "null" {
				continue
			}
			issues = append(issues, githubIssueListEntry{ID: entry.ID, GitHubIssue: GitHubIssue{Number: entry.Number, Title: entry.Title, State: entry.State, URL: entry.URL, Author: entry.Author, Labels: entry.Labels, Assignees: entry.Assignees}})
		}
	}
	return issues, nil
}

func githubIssueHasAnyLabel(issue GitHubIssue, selected []string) bool {
	wanted := make(map[string]struct{}, len(selected))
	for _, label := range selected {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			wanted[label] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return true
	}
	for _, label := range issue.Labels {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(label.Name))]; ok {
			return true
		}
	}
	return false
}
