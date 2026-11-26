package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

const (
	githubIssueListLimit             = 100
	githubIssueSearchLimit           = 1000
	githubIssueLinkedBranchBatchSize = 100
)

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

type githubIssueListEntry struct {
	GitHubIssue
	ID string `json:"id"`
}

type githubDevelopmentAssociationNode struct {
	ID             string `json:"id"`
	LinkedBranches struct {
		TotalCount int `json:"totalCount"`
	} `json:"linkedBranches"`
	ClosedByPullRequestsReferences struct {
		TotalCount int `json:"totalCount"`
	} `json:"closedByPullRequestsReferences"`
	IssueDependenciesSummary struct {
		BlockedBy int `json:"blockedBy"`
	} `json:"issueDependenciesSummary"`
}

type githubLinkedBranchResponse struct {
	Data struct {
		Nodes []githubDevelopmentAssociationNode `json:"nodes"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type githubIssueSearchCountResponse struct {
	TotalCount        int  `json:"total_count"`
	IncompleteResults bool `json:"incomplete_results"`
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

	allowed, err := s.RepositoryAllowed(r.Context(), repository)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, errors.New("repository is disabled in Settings"))
		return
	}
	r = r.WithContext(githubutil.WithCaller(r.Context(), "issue_browser"))

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
	branch := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("branch")))
	if branch != "" && branch != "all" && branch != "with" && branch != "without" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "branch must be all, with, or without"})
		return
	}
	if branch == "all" {
		branch = ""
	}
	issues, err := s.listGitHubIssues(r.Context(), repository, state, search, assignee, branch)
	if err != nil {
		status := githubErrorStatus(w, http.StatusBadGateway, err)
		if status != http.StatusTooManyRequests {
			s.logf("localruntime.ui status=warn endpoint=github_issues_list repo=%s err=%q", repository, err)
		}
		writeJSONError(w, status, fmt.Errorf("list GitHub issues: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "issues": issues})
}

func (s Server) listGitHubIssues(ctx context.Context, repository, state, search, assignee, branch string) ([]GitHubIssue, error) {
	fields := "number,title,state,url,author,labels,assignees,createdAt,updatedAt"
	limit := githubIssueListLimit
	if branch != "" {
		fields = "id," + fields
	}
	linked := make(map[string]bool)
	for {
		args := []string{
			"issue", "list", "--repo", repository, "--state", state,
			"--limit", strconv.Itoa(limit),
			"--json", fields,
		}
		if search != "" {
			args = append(args, "--search", search)
		}
		if assignee == "me" {
			args = append(args, "--assignee", "@me")
		}
		result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: args})
		if err != nil {
			return nil, err
		}
		var entries []githubIssueListEntry
		if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &entries); err != nil {
			return nil, fmt.Errorf("decode GitHub issues: %w", err)
		}
		issues := make([]GitHubIssue, 0, min(len(entries), githubIssueListLimit))
		if branch == "" || len(entries) == 0 {
			for _, entry := range entries {
				issues = append(issues, entry.GitHubIssue)
			}
			return issues, nil
		}
		unchecked := make([]githubIssueListEntry, 0, len(entries))
		for _, entry := range entries {
			if _, ok := linked[entry.ID]; !ok {
				unchecked = append(unchecked, entry)
			}
		}
		newLinked, err := s.githubIssuesWithLinkedBranches(ctx, unchecked)
		if err != nil {
			return nil, err
		}
		for id, hasLinkedBranch := range newLinked {
			linked[id] = hasLinkedBranch
		}
		for _, entry := range entries {
			if (branch == "with") == linked[entry.ID] {
				issues = append(issues, entry.GitHubIssue)
				if len(issues) == githubIssueListLimit {
					return issues, nil
				}
			}
		}
		if len(entries) < limit {
			return issues, nil
		}
		if search != "" && limit == githubIssueSearchLimit {
			count, err := s.githubIssueSearchCount(ctx, repository, state, search, assignee)
			if err != nil {
				return nil, err
			}
			if count > githubIssueSearchLimit {
				return nil, fmt.Errorf("GitHub search returned more than its %d-issue maximum; narrow the search to filter by linked branch", githubIssueSearchLimit)
			}
			return issues, nil
		}
		maxInt := int(^uint(0) >> 1)
		if limit > maxInt/2 {
			return nil, fmt.Errorf("too many GitHub issues to filter by linked branch")
		}
		limit *= 2
		if search != "" {
			limit = min(limit, githubIssueSearchLimit)
		}
	}
}

func (s Server) githubIssueSearchCount(ctx context.Context, repository, state, search, assignee string) (int, error) {
	query := search + " repo:" + repository + " is:issue"
	if state != "all" {
		query += " state:" + state
	}
	if assignee == "me" {
		query += " assignee:@me"
	}
	result, err := s.runIssueGH(ctx, execx.Command{
		Name: "gh",
		Args: []string{"api", "--method", "GET", "search/issues", "-f", "q=" + query, "-F", "per_page=1"},
	})
	if err != nil {
		return 0, fmt.Errorf("count searched GitHub issues: %w", err)
	}
	var response githubIssueSearchCountResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &response); err != nil {
		return 0, fmt.Errorf("decode searched GitHub issue count: %w", err)
	}
	if response.IncompleteResults {
		return 0, fmt.Errorf("count searched GitHub issues: GitHub returned incomplete results")
	}
	return response.TotalCount, nil
}

func (s Server) githubIssuesWithLinkedBranches(ctx context.Context, issues []githubIssueListEntry) (map[string]bool, error) {
	states, err := s.githubIssueDevelopmentStates(ctx, issues)
	if err != nil {
		return nil, err
	}
	linked := make(map[string]bool, len(states))
	for id, state := range states {
		linked[id] = state.Linked
	}
	return linked, nil
}

type githubIssueDevelopmentState struct {
	Linked  bool
	Blocked bool
}

func (s Server) githubIssueDevelopmentStates(ctx context.Context, issues []githubIssueListEntry) (map[string]githubIssueDevelopmentState, error) {
	const query = `query($ids:[ID!]!){nodes(ids:$ids){... on Issue{id linkedBranches(first:1){totalCount} closedByPullRequestsReferences(first:1,includeClosedPrs:true){totalCount} issueDependenciesSummary{blockedBy}}}}`
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == "" {
			return nil, fmt.Errorf("GitHub issue %d is missing its node ID", issue.Number)
		}
	}
	states := make(map[string]githubIssueDevelopmentState, len(issues))
	for start := 0; start < len(issues); start += githubIssueLinkedBranchBatchSize {
		end := min(start+githubIssueLinkedBranchBatchSize, len(issues))
		args := []string{"api", "graphql", "-f", "query=" + query}
		for _, issue := range issues[start:end] {
			args = append(args, "-F", "ids[]="+issue.ID)
		}
		result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: args})
		if err != nil {
			return nil, fmt.Errorf("list linked GitHub issue branches: %w", err)
		}
		var response githubLinkedBranchResponse
		if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &response); err != nil {
			return nil, fmt.Errorf("decode linked GitHub issue branches: %w", err)
		}
		if len(response.Errors) > 0 {
			return nil, fmt.Errorf("list linked GitHub issue branches: %s", response.Errors[0].Message)
		}
		for _, node := range response.Data.Nodes {
			if node.ID != "" {
				states[node.ID] = githubIssueDevelopmentState{
					Linked:  node.LinkedBranches.TotalCount > 0 || node.ClosedByPullRequestsReferences.TotalCount > 0,
					Blocked: node.IssueDependenciesSummary.BlockedBy > 0,
				}
			}
		}
	}
	if len(states) != len(issues) {
		return nil, fmt.Errorf("GitHub returned development data for %d of %d issues", len(states), len(issues))
	}
	return states, nil
}

func (s Server) handleGitHubIssueDetail(w http.ResponseWriter, r *http.Request, repository string, number int) {
	issue, err := s.fetchGitHubIssue(r.Context(), repository, number)
	if err != nil {
		writeJSONError(w, githubErrorStatus(w, http.StatusBadGateway, err), err)
		return
	}
	response := map[string]any{"ok": true, "issue": issue}
	if s.IssueClaims != nil && strings.EqualFold(issue.State, "open") {
		records, _ := s.IssueClaims.List()
		var latest localruntime.IssueClaimRecord
		for _, record := range records {
			key := record.Run.GitHubIssue
			if !strings.EqualFold(key.Repository, repository) || key.Number != number {
				continue
			}
			if latest.ID == "" || key.Claim.Attempt > latest.Run.GitHubIssue.Claim.Attempt ||
				(key.Claim.Attempt == latest.Run.GitHubIssue.Claim.Attempt && record.UpdatedAt.After(latest.UpdatedAt)) {
				latest = record
			}
		}
		if latest.Incident == workflowIssueRecoveryIncident ||
			(latest.Incident == localruntime.IssueIncidentBranchRecovery && hasUnsubmittedWorkflowRecovery(latest)) {
			response["recoveryReason"] = latest.Error
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s Server) fetchGitHubIssueMetadata(ctx context.Context, repository string, number int) (GitHubIssue, error) {
	args := []string{
		"issue", "view", strconv.Itoa(number), "--repo", repository,
		"--json", "number,title,body,state,url,author,labels,assignees,createdAt,updatedAt",
	}
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: args})
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
	return issue, nil
}

func (s Server) fetchGitHubIssue(ctx context.Context, repository string, number int) (GitHubIssue, error) {
	issue, err := s.fetchGitHubIssueMetadata(ctx, repository, number)
	if err != nil {
		return GitHubIssue{}, err
	}
	commentResult, err := s.runIssueGH(ctx, execx.Command{
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

func githubErrorStatus(w http.ResponseWriter, fallback int, err error) int {
	var rate *githubutil.RateLimitError
	if errors.As(err, &rate) {
		w.Header().Set("Retry-After", rate.RetryAt.UTC().Format(http.TimeFormat))
		return http.StatusTooManyRequests
	}
	return fallback
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
	if !config.ValidGitHubRepository(value) {
		return "", false
	}
	return value, true
}

func (s Server) handleGitHubIssuesPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/issues" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/#studio-issues", http.StatusFound)
}
