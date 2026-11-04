package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

const githubIssueListLimit = 100

// GitHubIssue is the issue data shown by the local issue browser.
type GitHubIssue struct {
	Number    int                  `json:"number"`
	Title     string               `json:"title"`
	Body      string               `json:"body,omitempty"`
	State     string               `json:"state"`
	URL       string               `json:"url"`
	Author    GitHubIssueUser      `json:"author"`
	Labels    []GitHubIssueLabel   `json:"labels,omitempty"`
	Assignees []GitHubIssueUser    `json:"assignees,omitempty"`
	CreatedAt string               `json:"createdAt,omitempty"`
	UpdatedAt string               `json:"updatedAt,omitempty"`
	Comments  []GitHubIssueComment `json:"comments,omitempty"`
}

type GitHubIssueUser struct {
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
}

type GitHubIssueLabel struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// handleGitHubLabels lists labels across repositories available to this user.
// Labels are an automation filter, so return a stable de-duplicated list.
func (s Server) handleGitHubLabels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	enabled, err := s.githubIssuesEnabled(r.Context())
	if err != nil || !enabled {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "GitHub Issues integration is turned off in Settings"})
		return
	}
	if s.GitHubIssueRunner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "GitHub issue runner is unavailable"})
		return
	}
	repos, err := s.resolveGitHubRepos(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("list GitHub repositories: %v", err)})
		return
	}
	labels := make(map[string]string)
	for _, repo := range repos {
		name, ok := validGitHubRepository(repo.FullName)
		if !ok {
			continue
		}
		result, err := s.GitHubIssueRunner.Run(r.Context(), execx.Command{Name: "gh", Args: []string{"label", "list", "--repo", name, "--limit", "1000", "--json", "name"}})
		if err != nil {
			continue
		}
		var entries []GitHubIssueLabel
		if json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &entries) != nil {
			continue
		}
		for _, entry := range entries {
			label := strings.TrimSpace(entry.Name)
			if label != "" {
				labels[strings.ToLower(label)] = label
			}
		}
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, label)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "labels": out})
}

type GitHubIssueComment struct {
	Author    GitHubIssueUser `json:"author"`
	Body      string          `json:"body"`
	CreatedAt string          `json:"createdAt,omitempty"`
	URL       string          `json:"url,omitempty"`
}

type githubIssueCommentResponse struct {
	User      GitHubIssueUser `json:"user"`
	Body      string          `json:"body"`
	CreatedAt string          `json:"created_at"`
	HTMLURL   string          `json:"html_url"`
}

func (s Server) handleGitHubIssues(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	enabled, err := s.githubIssuesEnabled(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": fmt.Sprintf("load GitHub Issues setting: %v", err)})
		return
	}
	if !enabled {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "GitHub Issues integration is turned off in Settings"})
		return
	}
	if s.GitHubIssueRunner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "GitHub issue runner is unavailable"})
		return
	}
	repository, ok := validGitHubRepository(r.URL.Query().Get("repo"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "repo must be OWNER/REPO"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/github/issues")
	if path == "" {
		s.handleGitHubIssueList(w, r, repository)
		return
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(strings.TrimPrefix(path, "/"), "/") {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "issue endpoint not found"})
		return
	}
	number, err := strconv.Atoi(strings.TrimPrefix(path, "/"))
	if err != nil || number <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "issue number must be positive"})
		return
	}
	s.handleGitHubIssueDetail(w, r, repository, number)
}

func (s Server) handleGitHubIssueList(w http.ResponseWriter, r *http.Request, repository string) {
	state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "closed" && state != "all" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "state must be open, closed, or all"})
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 256 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "search must be 256 characters or fewer"})
		return
	}
	assignee := strings.TrimSpace(r.URL.Query().Get("assignee"))
	if assignee != "" && assignee != "me" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "assignee must be me"})
		return
	}
	issues, err := s.listGitHubIssues(r.Context(), repository, state, search, assignee)
	if err != nil {
		s.logf("localruntime.ui status=warn endpoint=github_issues_list repo=%s err=%q", repository, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("list GitHub issues: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "issues": issues})
}

func (s Server) listGitHubIssues(ctx context.Context, repository, state, search, assignee string) ([]GitHubIssue, error) {
	args := []string{
		"issue", "list", "--repo", repository, "--state", state,
		"--limit", strconv.Itoa(githubIssueListLimit),
		"--json", "number,title,state,url,author,labels,assignees,createdAt,updatedAt",
	}
	if search != "" {
		args = append(args, "--search", search)
	}
	if assignee == "me" {
		args = append(args, "--assignee", "@me")
	}
	result, err := s.GitHubIssueRunner.Run(ctx, execx.Command{Name: "gh", Args: args})
	if err != nil {
		return nil, err
	}
	var issues []GitHubIssue
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &issues); err != nil {
		return nil, fmt.Errorf("decode GitHub issues: %w", err)
	}
	if issues == nil {
		issues = []GitHubIssue{}
	}
	return issues, nil
}

func (s Server) handleGitHubIssueDetail(w http.ResponseWriter, r *http.Request, repository string, number int) {
	issue, err := s.fetchGitHubIssue(r.Context(), repository, number)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "issue": issue})
}

func (s Server) fetchGitHubIssue(ctx context.Context, repository string, number int) (GitHubIssue, error) {
	args := []string{
		"issue", "view", strconv.Itoa(number), "--repo", repository,
		"--json", "number,title,body,state,url,author,labels,assignees,createdAt,updatedAt",
	}
	result, err := s.GitHubIssueRunner.Run(ctx, execx.Command{Name: "gh", Args: args})
	if err != nil {
		return GitHubIssue{}, fmt.Errorf("view GitHub issue: %w", err)
	}
	var issue GitHubIssue
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &issue); err != nil {
		return GitHubIssue{}, fmt.Errorf("decode GitHub issue: %w", err)
	}
	if issue.Number != number {
		return GitHubIssue{}, fmt.Errorf("GitHub returned a different issue number")
	}
	commentResult, err := s.GitHubIssueRunner.Run(ctx, execx.Command{
		Name: "gh",
		Args: []string{"api", "--paginate", fmt.Sprintf("repos/%s/issues/%d/comments", repository, number)},
	})
	if err != nil {
		return GitHubIssue{}, fmt.Errorf("load GitHub issue comments: %w", err)
	}
	if err := decodePaginatedGitHubIssueComments(commentResult.Stdout, &issue); err != nil {
		return GitHubIssue{}, fmt.Errorf("decode GitHub issue comments: %w", err)
	}
	return issue, nil
}

func decodePaginatedGitHubIssueComments(raw string, issue *GitHubIssue) error {
	if issue == nil {
		return fmt.Errorf("issue is required")
	}
	if strings.TrimSpace(raw) == "" {
		issue.Comments = []GitHubIssueComment{}
		return nil
	}
	trimmed := strings.TrimSpace(raw)
	var pages [][]githubIssueCommentResponse
	if err := json.Unmarshal([]byte(trimmed), &pages); err == nil {
		for _, page := range pages {
			appendGitHubIssueComments(issue, page)
		}
		if issue.Comments == nil {
			issue.Comments = []GitHubIssueComment{}
		}
		return nil
	}

	// gh versions without --slurp print each paginated JSON response as its own
	// top-level array. Decode the JSON stream page by page to support those
	// versions as well as the slurped array format above.
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	for {
		var page []githubIssueCommentResponse
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		appendGitHubIssueComments(issue, page)
	}
	if issue.Comments == nil {
		issue.Comments = []GitHubIssueComment{}
	}
	return nil
}

func appendGitHubIssueComments(issue *GitHubIssue, comments []githubIssueCommentResponse) {
	for _, comment := range comments {
		issue.Comments = append(issue.Comments, GitHubIssueComment{
			Author: comment.User, Body: comment.Body, CreatedAt: comment.CreatedAt, URL: comment.HTMLURL,
		})
	}
}

func validGitHubRepository(value string) (string, bool) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validGitHubRepositoryPart(parts[0]) || !validGitHubRepositoryPart(parts[1]) {
		return "", false
	}
	return value, true
}

func validGitHubRepositoryPart(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

func (s Server) handleGitHubIssuesPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/issues" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/#studio-issues", http.StatusFound)
}
