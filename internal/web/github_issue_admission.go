package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

const githubIssueCommandTimeout = 30 * time.Second

func (s Server) runIssueGH(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	if s.GitHubIssueRunner == nil {
		return execx.Result{}, fmt.Errorf("GitHub issue runner is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, githubIssueCommandTimeout)
	defer cancel()
	return s.GitHubCooldown.Run(ctx, s.GitHubIssueRunner, cmd, s.IssueRecovery != nil)
}

func (s Server) issueGitHubLogin(ctx context.Context) (string, error) {
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "--cache", "10m", "user"}})
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
