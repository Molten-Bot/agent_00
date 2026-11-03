package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
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

type GitHubIssueComment struct {
	Author    GitHubIssueUser `json:"author"`
	Body      string          `json:"body"`
	CreatedAt string          `json:"createdAt,omitempty"`
	URL       string          `json:"url,omitempty"`
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
	args := []string{
		"issue", "list", "--repo", repository, "--state", state,
		"--limit", strconv.Itoa(githubIssueListLimit),
		"--json", "number,title,state,url,author,labels,assignees,createdAt,updatedAt",
	}
	if search != "" {
		args = append(args, "--search", search)
	}
	result, err := s.GitHubIssueRunner.Run(r.Context(), execx.Command{Name: "gh", Args: args})
	if err != nil {
		s.logf("localruntime.ui status=warn endpoint=github_issues_list repo=%s err=%q", repository, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("list GitHub issues: %v", err)})
		return
	}
	var issues []GitHubIssue
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &issues); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("decode GitHub issues: %v", err)})
		return
	}
	if issues == nil {
		issues = []GitHubIssue{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "issues": issues})
}

func (s Server) handleGitHubIssueDetail(w http.ResponseWriter, r *http.Request, repository string, number int) {
	args := []string{
		"issue", "view", strconv.Itoa(number), "--repo", repository,
		"--json", "number,title,body,state,url,author,labels,assignees,createdAt,updatedAt",
	}
	result, err := s.GitHubIssueRunner.Run(r.Context(), execx.Command{Name: "gh", Args: args})
	if err != nil {
		s.logf("localruntime.ui status=warn endpoint=github_issue repo=%s issue=%d err=%q", repository, number, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("view GitHub issue: %v", err)})
		return
	}
	var issue GitHubIssue
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &issue); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("decode GitHub issue: %v", err)})
		return
	}
	if issue.Number != number {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "GitHub returned a different issue number"})
		return
	}
	commentResult, err := s.GitHubIssueRunner.Run(r.Context(), execx.Command{
		Name: "gh",
		Args: []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments", repository, number)},
	})
	if err != nil {
		s.logf("localruntime.ui status=warn endpoint=github_issue_comments repo=%s issue=%d err=%q", repository, number, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("load GitHub issue comments: %v", err)})
		return
	}
	if err := decodePaginatedGitHubIssueComments(commentResult.Stdout, &issue); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("decode GitHub issue comments: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "issue": issue})
}

func decodePaginatedGitHubIssueComments(raw string, issue *GitHubIssue) error {
	if issue == nil {
		return fmt.Errorf("issue is required")
	}
	if strings.TrimSpace(raw) == "" {
		issue.Comments = []GitHubIssueComment{}
		return nil
	}
	var pages [][]struct {
		User      GitHubIssueUser `json:"user"`
		Body      string          `json:"body"`
		CreatedAt string          `json:"created_at"`
		HTMLURL   string          `json:"html_url"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &pages); err != nil {
		return err
	}
	for _, page := range pages {
		for _, comment := range page {
			issue.Comments = append(issue.Comments, GitHubIssueComment{
				Author: comment.User, Body: comment.Body, CreatedAt: comment.CreatedAt, URL: comment.HTMLURL,
			})
		}
	}
	if issue.Comments == nil {
		issue.Comments = []GitHubIssueComment{}
	}
	return nil
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
	content := `<link rel="stylesheet" href="/static/github-issues.css">
<section class="github-issues-page" aria-labelledby="github-issues-title">
  <header class="github-issues-header">
    <div><h1 id="github-issues-title">GitHub Issues</h1><p>Browse an issue and start a repository task that produces a pull request.</p></div>
    <a class="github-issues-back" href="/">Back to workspace</a>
  </header>
  <p id="github-issues-status" class="github-issues-status" role="status" aria-live="polite">Loading GitHub Issues settings…</p>
  <div id="github-issues-disabled" class="github-issues-disabled" hidden>Turn on GitHub Issues in <a href="/#settings">Settings</a> to browse issues.</div>
  <div id="github-issues-browser" hidden>
    <form id="github-issues-filters" class="github-issues-filters">
      <label>Repository<select id="github-issues-repo" required><option value="">Loading repositories…</option></select></label>
      <label>State<select id="github-issues-state"><option value="open">Open</option><option value="closed">Closed</option><option value="all">All</option></select></label>
      <label class="github-issues-search">Search<input id="github-issues-search" type="search" maxlength="256" placeholder="Search this repository" autocomplete="off"></label>
      <button type="submit">Search</button>
    </form>
    <div class="github-issues-columns">
      <section class="github-issues-list-panel" aria-labelledby="github-issues-list-title"><h2 id="github-issues-list-title">Issues</h2><ol id="github-issues-list" class="github-issues-list"></ol></section>
      <article id="github-issue-detail" class="github-issue-detail" aria-live="polite"><p>Select an issue to see its details and conversation.</p></article>
    </div>
  </div>
</section>
<script src="/static/github-issues.js" defer></script>`
	s.renderSitePage(r.Context(), w, sitePageData{
		Title: "GitHub Issues · agent_00", BodyClass: "github-issues-body", PageClass: "github-issues-page-shell", MainClass: "github-issues-main", Heading: "GitHub Issues", Content: template.HTML(content),
	})
}
