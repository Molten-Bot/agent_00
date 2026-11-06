package web

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

const githubIssueAutoStartPollInterval = time.Minute

type githubIssueAutoStartRunConfig struct {
	RepoURL     string                    `json:"repoUrl"`
	BaseBranch  string                    `json:"baseBranch,omitempty"`
	Prompt      string                    `json:"prompt"`
	GitHubIssue *config.GitHubIssueConfig `json:"githubIssue"`
}

// runGitHubIssueAutoStart checks for new matching open issues while the local
// UI service is running. The first scan after enabling or changing filters
// establishes a baseline and does not launch old issues.
func (s Server) runGitHubIssueAutoStart(ctx context.Context) {
	if s.GitHubIssueRunner == nil || s.SubmitLocalPrompt == nil || s.ReviewSettingsStatus == nil {
		return
	}
	ticker := time.NewTicker(githubIssueAutoStartPollInterval)
	defer ticker.Stop()
	seen := make(map[string]bool)
	baseline := true
	filters := ""
	for {
		s.pollGitHubIssueAutoStart(ctx, seen, &baseline, &filters)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s Server) pollGitHubIssueAutoStart(ctx context.Context, seen map[string]bool, baseline *bool, filters *string) {
	state, err := s.ReviewSettingsStatus(ctx)
	if err != nil {
		s.logf("localruntime.ui status=warn event=github_issue_auto_start_settings err=%q", err)
		return
	}
	if !state.GitHubIssuesEnabled || !state.GitHubIssueAutoStartAssigned {
		*baseline = true
		*filters = ""
		return
	}
	nextFilters := githubIssueAutoStartFilterKey(state.GitHubIssueAutoStartOnlyAssigned, state.GitHubIssueAutoStartLabels)
	if nextFilters != *filters {
		*baseline = true
		*filters = nextFilters
	}
	if err := s.scanGitHubIssuesForAutoStart(ctx, *baseline, seen, state.GitHubIssueLinkMode, state.GitHubIssueAutoStartOnlyAssigned, state.GitHubIssueAutoStartLabels); err != nil {
		s.logf("localruntime.ui status=warn event=github_issue_auto_start_scan err=%q", err)
		return
	}
	*baseline = false
}

func githubIssueAutoStartFilterKey(onlyAssigned bool, labels []string) string {
	unique := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			unique[label] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(unique))
	for label := range unique {
		normalized = append(normalized, label)
	}
	sort.Strings(normalized)
	encoded, _ := json.Marshal(normalized)
	return fmt.Sprintf("%t:%s", onlyAssigned, encoded)
}

func (s Server) scanGitHubIssuesForAutoStart(ctx context.Context, baseline bool, seen map[string]bool, linkMode string, onlyAssigned bool, requiredLabels []string) error {
	repos, err := s.resolveGitHubRepos(ctx)
	if err != nil {
		return fmt.Errorf("resolve GitHub repositories: %w", err)
	}
	var errs []string
	for _, repo := range repos {
		fullName, ok := validGitHubRepository(repo.FullName)
		if !ok {
			continue
		}
		issues, err := s.listGitHubIssuesForAutoStart(ctx, fullName, onlyAssigned)
		if err != nil {
			errs = append(errs, fmt.Sprintf("list issues in %s: %v", fullName, err))
			continue
		}
		for _, issue := range issues {
			if !githubIssueHasAllLabels(issue, requiredLabels) {
				continue
			}
			key := fullName + "#" + fmt.Sprint(issue.Number)
			if seen[key] {
				continue
			}
			if baseline {
				seen[key] = true
				continue
			}
			fullIssue, err := s.fetchGitHubIssue(ctx, fullName, issue.Number)
			if err != nil {
				errs = append(errs, fmt.Sprintf("load %s: %v", key, err))
				continue
			}
			url := strings.TrimSpace(fullIssue.URL)
			if url == "" {
				url = fmt.Sprintf("https://github.com/%s/issues/%d", fullName, fullIssue.Number)
			}
			mode := config.GitHubIssueLinkReference
			if strings.EqualFold(strings.TrimSpace(linkMode), config.GitHubIssueLinkClose) {
				mode = config.GitHubIssueLinkClose
			}
			run := githubIssueAutoStartRunConfig{
				RepoURL:    repo.HTMLURL,
				BaseBranch: repo.DefaultBranch,
				Prompt:     buildGitHubIssueAutoStartPrompt(fullName, fullIssue),
				GitHubIssue: &config.GitHubIssueConfig{
					Repository: fullName,
					Number:     fullIssue.Number,
					URL:        url,
					LinkMode:   mode,
				},
			}
			if strings.TrimSpace(run.RepoURL) == "" {
				run.RepoURL = "https://github.com/" + fullName
			}
			body, err := json.Marshal(run)
			if err != nil {
				errs = append(errs, fmt.Sprintf("encode %s: %v", key, err))
				continue
			}
			requestID, err := s.SubmitLocalPrompt(ctx, body)
			if err != nil {
				errs = append(errs, fmt.Sprintf("start %s: %v", key, err))
				continue
			}
			seen[key] = true
			s.logf("localruntime.ui status=ok event=github_issue_auto_start repo=%s issue=%d request_id=%s", fullName, fullIssue.Number, requestID)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (s Server) listGitHubIssuesForAutoStart(ctx context.Context, repository string, onlyAssigned bool) ([]GitHubIssue, error) {
	args := assignedIssueListArgs(repository)
	if !onlyAssigned {
		args = []string{"issue", "list", "--repo", repository, "--state", "open", "--limit", "1000", "--json", "number,title,state,url,author,labels,assignees,createdAt,updatedAt"}
	}
	result, err := s.GitHubIssueRunner.Run(ctx, execx.Command{Name: "gh", Args: args})
	if err != nil {
		return nil, err
	}
	var issues []GitHubIssue
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &issues); err != nil {
		return nil, fmt.Errorf("decode GitHub issues: %w", err)
	}
	return issues, nil
}

func githubIssueHasAllLabels(issue GitHubIssue, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		have[strings.ToLower(strings.TrimSpace(label.Name))] = struct{}{}
	}
	for _, label := range required {
		if _, ok := have[strings.ToLower(strings.TrimSpace(label))]; !ok {
			return false
		}
	}
	return true
}

func buildGitHubIssueAutoStartPrompt(repository string, issue GitHubIssue) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Complete GitHub issue %s#%d: %s\n\n", repository, issue.Number, issue.Title)
	fmt.Fprintf(&prompt, "Issue URL: %s\nState: %s\nAuthor: @%s\n", issue.URL, issue.State, issue.Author.Login)
	if len(issue.Labels) > 0 {
		labels := make([]string, 0, len(issue.Labels))
		for _, label := range issue.Labels {
			labels = append(labels, label.Name)
		}
		fmt.Fprintf(&prompt, "Labels: %s\n", strings.Join(labels, ", "))
	}
	prompt.WriteString("\n## Issue description\n")
	prompt.WriteString(strings.TrimSpace(issue.Body))
	if len(issue.Comments) > 0 {
		prompt.WriteString("\n\n## Issue comments\n")
		for _, comment := range issue.Comments {
			fmt.Fprintf(&prompt, "\n### @%s\n%s\n", comment.Author.Login, strings.TrimSpace(comment.Body))
		}
	}
	prompt.WriteString("\n\nImplement the requested change in the repository. Follow the repository's instructions, run the relevant checks, and summarize the result.")
	return strings.TrimSpace(prompt.String())
}

// assignedIssueListArgs mirrors the UI issue list query while asking gh to
// resolve @me against its authenticated account.
func assignedIssueListArgs(repository string) []string {
	return []string{"issue", "list", "--repo", repository, "--state", "open", "--limit", "1000", "--assignee", "@me", "--json", "number,title,state,url,author,labels,assignees,createdAt,updatedAt"}
}
