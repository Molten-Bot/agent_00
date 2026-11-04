package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

type githubIssueRunnerFunc func(context.Context, execx.Command) (execx.Result, error)

func (f githubIssueRunnerFunc) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	return f(ctx, cmd)
}

func githubIssueTestServer(runner execx.Runner, enabled bool) Server {
	return Server{
		Broker:            NewBroker(),
		GitHubIssueRunner: runner,
		ReviewSettingsStatus: func(context.Context) (ReviewSettingsState, error) {
			return ReviewSettingsState{GitHubIssuesEnabled: enabled}, nil
		},
	}
}

func TestGitHubIssueListUsesGhFiltersAndDecodesIssues(t *testing.T) {
	t.Parallel()
	var got execx.Command
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		got = cmd
		return execx.Result{Stdout: `[{"number":17,"title":"Add issue browser","state":"OPEN","url":"https://github.com/acme/repo/issues/17","author":{"login":"octocat"},"labels":[{"name":"feature"}],"assignees":[{"login":"octocat"}]}]`}, nil
	}), true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo&search=browser", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	wantArgs := []string{"issue", "list", "--repo", "acme/repo", "--state", "open", "--limit", "100", "--json", "number,title,state,url,author,labels,assignees,createdAt,updatedAt", "--search", "browser"}
	if got.Name != "gh" || !reflect.DeepEqual(got.Args, wantArgs) {
		t.Fatalf("command = %s %v, want gh %v", got.Name, got.Args, wantArgs)
	}
	if !strings.Contains(response.Body.String(), `"title":"Add issue browser"`) || !strings.Contains(response.Body.String(), `"login":"octocat"`) {
		t.Fatalf("response did not include issue data: %s", response.Body.String())
	}
}

func TestGitHubIssueListCanFilterAssignedToMe(t *testing.T) {
	t.Parallel()
	var got execx.Command
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		got = cmd
		return execx.Result{Stdout: `[]`}, nil
	}), true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo&assignee=me", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	want := []string{"issue", "list", "--repo", "acme/repo", "--state", "open", "--limit", "100", "--json", "number,title,state,url,author,labels,assignees,createdAt,updatedAt", "--assignee", "@me"}
	if !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("args = %v, want %v", got.Args, want)
	}
}

func TestAssignedIssueAutoStartBaselinesAndSubmitsNewIssueOnce(t *testing.T) {
	t.Parallel()
	var submitted [][]byte
	listCalls := 0
	assigned := false
	srv := NewServer("", NewBroker())
	srv.GitHubIssueRunner = githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if len(cmd.Args) >= 2 && cmd.Args[0] == "issue" && cmd.Args[1] == "list" {
			listCalls++
			if !assigned {
				return execx.Result{Stdout: `[{"number":22,"title":"Already assigned","state":"OPEN","url":"https://github.com/acme/repo/issues/22","author":{"login":"octocat"}}]`}, nil
			}
			return execx.Result{Stdout: `[{"number":22,"title":"Already assigned","state":"OPEN","url":"https://github.com/acme/repo/issues/22","author":{"login":"octocat"}},{"number":23,"title":"Add assigned automation","state":"OPEN","url":"https://github.com/acme/repo/issues/23","author":{"login":"octocat"}}]`}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "issue" && cmd.Args[1] == "view" {
			return execx.Result{Stdout: `{"number":23,"title":"Add assigned automation","body":"Build it","state":"OPEN","url":"https://github.com/acme/repo/issues/23","author":{"login":"octocat"},"labels":[{"name":"feature"}]}`}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "api" {
			return execx.Result{Stdout: `[{"user":{"login":"reviewer"},"body":"Include a test.","created_at":"2026-02-01T00:00:00Z"}]`}, nil
		}
		t.Fatalf("unexpected gh command: %#v", cmd)
		return execx.Result{}, nil
	})
	srv.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		return []GitHubRepo{{FullName: "acme/repo", HTMLURL: "https://github.com/acme/repo", DefaultBranch: "main"}}, nil
	}
	srv.SubmitLocalPrompt = func(_ context.Context, body []byte) (string, error) {
		submitted = append(submitted, append([]byte(nil), body...))
		return "request-1", nil
	}
	seen := make(map[string]bool)
	if err := srv.scanAssignedGitHubIssues(context.Background(), true, seen, config.GitHubIssueLinkClose); err != nil {
		t.Fatalf("baseline scan error: %v", err)
	}
	if len(submitted) != 0 || !seen["acme/repo#22"] {
		t.Fatalf("baseline submitted %d tasks, seen=%v", len(submitted), seen)
	}
	assigned = true
	if err := srv.scanAssignedGitHubIssues(context.Background(), false, seen, config.GitHubIssueLinkClose); err != nil {
		t.Fatalf("new issue scan error: %v", err)
	}
	if err := srv.scanAssignedGitHubIssues(context.Background(), false, seen, config.GitHubIssueLinkClose); err != nil {
		t.Fatalf("repeat issue scan error: %v", err)
	}
	if len(submitted) != 1 || listCalls != 3 {
		t.Fatalf("submitted tasks/list calls = %d/%d, want 1/3", len(submitted), listCalls)
	}
	var run struct {
		RepoURL     string                   `json:"repoUrl"`
		BaseBranch  string                   `json:"baseBranch"`
		Prompt      string                   `json:"prompt"`
		GitHubIssue config.GitHubIssueConfig `json:"githubIssue"`
	}
	if err := json.Unmarshal(submitted[0], &run); err != nil {
		t.Fatalf("decode submitted run: %v", err)
	}
	if run.RepoURL != "https://github.com/acme/repo" || run.BaseBranch != "main" || run.GitHubIssue.Number != 23 || run.GitHubIssue.LinkMode != config.GitHubIssueLinkClose || !strings.Contains(run.Prompt, "Build it") || !strings.Contains(run.Prompt, "Include a test.") {
		t.Fatalf("submitted run omitted issue context or metadata: %#v", run)
	}
}

func TestGitHubIssueListRejectsInvalidFiltersBeforeCallingGh(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		calls++
		return execx.Result{}, nil
	}), true)
	for _, path := range []string{
		"/api/github/issues?repo=acme/repo&state=unexpected",
		"/api/github/issues?repo=../repo",
		"/api/github/issues?repo=acme/repo&search=" + strings.Repeat("x", 257),
		"/api/github/issues?repo=acme/repo&assignee=someone-else",
	} {
		response := httptest.NewRecorder()
		srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, response.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("gh calls = %d, want zero for invalid input", calls)
	}
}

func TestGitHubIssueDetailIncludesComments(t *testing.T) {
	t.Parallel()
	var got []execx.Command
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		got = append(got, cmd)
		if cmd.Name == "gh" && len(cmd.Args) > 1 && cmd.Args[0] == "issue" {
			return execx.Result{Stdout: `{"number":17,"title":"Add issue browser","body":"Description","state":"OPEN","url":"https://github.com/acme/repo/issues/17","author":{"login":"octocat"},"labels":[{"name":"feature"}],"assignees":[{"login":"octocat"}]}`}, nil
		}
		return execx.Result{Stdout: `[{"user":{"login":"hubot"},"body":"Please add tests","created_at":"2026-01-01T00:00:00Z"}]
[{"user":{"login":"octocat"},"body":"Second page comment","created_at":"2026-01-02T00:00:00Z"}]`}, nil
	}), true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues/17?repo=acme/repo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	wantCommands := [][]string{
		{"issue", "view", "17", "--repo", "acme/repo", "--json", "number,title,body,state,url,author,labels,assignees,createdAt,updatedAt"},
		{"api", "--paginate", "repos/acme/repo/issues/17/comments"},
	}
	if len(got) != len(wantCommands) {
		t.Fatalf("command count = %d, want %d: %#v", len(got), len(wantCommands), got)
	}
	for index, wantArgs := range wantCommands {
		if !reflect.DeepEqual(got[index].Args, wantArgs) {
			t.Errorf("command %d args = %v, want %v", index, got[index].Args, wantArgs)
		}
	}
	if !strings.Contains(response.Body.String(), `"body":"Please add tests"`) || !strings.Contains(response.Body.String(), `"body":"Second page comment"`) {
		t.Fatalf("response omitted paginated issue comments: %s", response.Body.String())
	}
}

func TestDecodePaginatedGitHubIssueCommentsAcceptsSlurpedAndEmptyResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{name: "slurped pages", raw: `[[{"user":{"login":"hubot"},"body":"first"}],[{"user":{"login":"octocat"},"body":"second"}]]`, want: 2},
		{name: "no comments", raw: `[]`, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := GitHubIssue{}
			if err := decodePaginatedGitHubIssueComments(tc.raw, &issue); err != nil {
				t.Fatalf("decodePaginatedGitHubIssueComments() error = %v", err)
			}
			if len(issue.Comments) != tc.want {
				t.Fatalf("comments count = %d, want %d", len(issue.Comments), tc.want)
			}
			if issue.Comments == nil {
				t.Fatal("comments is nil, want an empty or populated slice")
			}
		})
	}
}

func TestGitHubIssueEndpointRejectsDisabledAndReportsGhErrors(t *testing.T) {
	t.Parallel()
	calls := 0
	runner := githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		calls++
		return execx.Result{}, errors.New("not logged in")
	})
	response := httptest.NewRecorder()
	githubIssueTestServer(runner, false).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo", nil))
	if response.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("disabled endpoint status/calls = %d/%d, want 403/0", response.Code, calls)
	}

	response = httptest.NewRecorder()
	githubIssueTestServer(runner, true).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo", nil))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "not logged in") || calls != 1 {
		t.Fatalf("runner error response = %d %s, calls %d", response.Code, response.Body.String(), calls)
	}
}

func TestGitHubIssuesPanelRendersInsideStudio(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(nil, true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	page := response.Body.String()
	for _, want := range []string{"GitHub Issues", "agent-00-header", "github-issues-dock-link", `href="#studio-issues"`, `data-prompt-title-mode="issues"`, `id="github-issues-panel"`, "github-issues-repo", "github-issues-search", "github-issues-count", "github-issues-assignee-filter", "user-round-arrow-left", "review-settings-github-issues-link-mode", "review-settings-github-issues-link-close", `href="/static/style.css"`, "github-issues.js"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(page, "github-issues.css") {
		t.Error("GitHub Issues should share the common stylesheet")
	}
	if strings.Contains(page, `id="github-issues-state"`) {
		t.Error("GitHub Issues should not render a state filter")
	}
	if strings.Contains(page, `id="github-issues-list-toggle"`) || strings.Contains(page, "Hide issue list") {
		t.Error("GitHub Issues should use the former list-toggle space for the issue display area")
	}
	if strings.Contains(page, `id="github-issues-title"`) || !strings.Contains(page, `data-prompt-title-label="GitHub Issues"`) {
		t.Error("GitHub Issues should use the shared Studio title without a duplicate issue heading")
	}
	if count := strings.Count(page, `data-lucide="folder-kanban"`); count < 4 {
		t.Errorf("folder-kanban icon appears %d times, want panel title, bottom dock, and settings controls", count)
	}
	libraryDockAt := strings.Index(page, `id="prompt-mode-library"`)
	issuesDockAt := strings.Index(page, `id="github-issues-dock-link"`)
	issuesDividerAt := -1
	if issuesDockAt >= 0 {
		if dividerOffset := strings.Index(page[issuesDockAt:], `class="prompt-mode-divider"`); dividerOffset >= 0 {
			issuesDividerAt = issuesDockAt + dividerOffset
		}
	}
	currentWorkDockAt := strings.Index(page, `id="task-view-dock-link"`)
	if libraryDockAt < 0 || issuesDockAt < libraryDockAt || issuesDividerAt < issuesDockAt || currentWorkDockAt < issuesDividerAt {
		t.Error("GitHub Issues should sit next to Library, before the divider and Current Work")
	}
	filtersAt := strings.Index(page, `id="github-issues-filters"`)
	repositoryAt := strings.Index(page, `id="github-issues-repo"`)
	if filtersAt < 0 || repositoryAt < filtersAt {
		t.Error("repository picker should remain in the issue filter row after removing the duplicate heading")
	}
	if !strings.Contains(page, `class="panel prompt-wrap brand-login-card-shell"`) || strings.Contains(page, `class="github-issues-page-shell"`) {
		t.Error("issues should render inside the shared Studio panel, not a separate page shell")
	}
	legacy := httptest.NewRecorder()
	srv.Handler().ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/issues", nil))
	if legacy.Code != http.StatusFound || legacy.Header().Get("Location") != "/#studio-issues" {
		t.Fatalf("legacy /issues route = %d %q, want redirect to Studio issue mode", legacy.Code, legacy.Header().Get("Location"))
	}
	asset := httptest.NewRecorder()
	srv.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/static/github-issues.js", nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("issue browser asset response = %d, want 200", asset.Code)
	}
	// Both task launch modes are rendered by the shared action row in index.html,
	// so the script only has to wire them up and submit the issue task.
	script := asset.Body.String()
	for _, want := range []string{"github-issue-edit-prompt", "github-issue-start", "enableTaskActions", "resetTaskActions", "submitTask", `state: "open"`, "open issues", "linkMode: githubIssueLinkMode", "githubIssue:", "loadingIssueNumber", "task-icons-breathe"} {
		if !strings.Contains(script, want) {
			t.Errorf("issue browser script is missing %q", want)
		}
	}
}

func TestGitHubIssuesTaskActionsReuseSharedPromptStyles(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(nil, true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	page := response.Body.String()
	for _, want := range []string{
		`id="github-issues-actions" class="prompt-actions"`,
		`<div class="prompt-actions-end">`,
		`id="github-issue-edit-prompt" class="prompt-action-button" type="button" aria-controls="github-issue-prompt-field" aria-expanded="false" disabled`,
		`id="github-issue-start" class="prompt-action-button prompt-submit" type="button" disabled`,
		`<span class="prompt-action-label">Edit prompt</span>`,
		`<span class="prompt-action-label">Start task</span>`,
		`id="github-issue-prompt-field" class="prompt-field"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("issue task actions are missing %q", want)
		}
	}
	browserAt := strings.Index(page, `id="github-issues-browser"`)
	promptAt := strings.Index(page, `id="github-issue-prompt-field"`)
	actionsAt := strings.Index(page, `id="github-issues-actions"`)
	editAt := strings.Index(page, `id="github-issue-edit-prompt"`)
	startAt := strings.Index(page, `id="github-issue-start"`)
	if browserAt < 0 || promptAt < browserAt || actionsAt < promptAt || editAt < actionsAt || startAt < editAt {
		t.Error("issue task buttons should sit in a trailing action row below the issue browser, edit before start")
	}
	stylesheet, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	if !strings.Contains(normalizeCSSWhitespace(string(stylesheet)), ".github-issues-panel > .prompt-field, .github-issues-panel > .prompt-actions { flex: 0 0 auto; }") {
		t.Error("expected the issue prompt and action row to dock at the bottom through the shared prompt layout")
	}
}

func TestGitHubIssuesPromptEditorAndStatusFollowSharedPromptBehavior(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(nil, true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	page := response.Body.String()
	if !strings.Contains(page, `aria-controls="github-issue-prompt-field"`) {
		t.Error("the edit prompt disclosure should name the field it expands")
	}
	if strings.Contains(page, `aria-label="Issue task prompt"`) {
		t.Error("the issue prompt textarea should keep the visible Prompt label instead of overriding it, like #builder-prompt-input")
	}

	stylesheet, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(stylesheet)
	if !strings.Contains(css, `.submit-status-inline[data-wrap="true"] {`) {
		t.Error("inline statuses should share one wrap opt-out so panel errors are readable instead of ellipsized")
	}
	if strings.Contains(css, `#local-prompt-status[data-wrap="true"] {`) {
		t.Error("the per-id wrap rule should be replaced by the shared .submit-status-inline rule")
	}

	asset := httptest.NewRecorder()
	srv.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/static/github-issues.js", nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("issue browser asset response = %d, want 200", asset.Code)
	}
	script := asset.Body.String()
	for _, want := range []string{
		// The disclosure collapses as well as expands, so aria-expanded stays truthful.
		"function openPromptEditor()",
		`if (editPromptButton.getAttribute("aria-expanded") === "true") closePromptEditor();`,
		"else openPromptEditor();",
		// Errors opt into wrapping because the action row is the panel's only status channel.
		`if (kind === "error") status.dataset.wrap = "true";`,
		// Turning the setting on mid-session restores the action row.
		"taskActions.hidden = !enabled;",
		"disabledRunFinished",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("issue browser script is missing %q", want)
		}
	}
}

func TestGitHubIssuesDockLinkFollowsSetting(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		srv := githubIssueTestServer(nil, enabled)
		response := httptest.NewRecorder()
		srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET / enabled=%t status = %d", enabled, response.Code)
		}
		page := response.Body.String()
		if enabled {
			if !strings.Contains(page, `id="github-issues-dock-link" class="prompt-mode-link" href="#studio-issues" aria-selected="false"`) {
				t.Fatalf("enabled dock link missing or disabled: %s", page)
			}
		} else if !strings.Contains(page, `id="github-issues-dock-link" class="prompt-mode-link site-dock-link is-disabled hidden"`) {
			t.Fatalf("disabled dock link is not hidden: %s", page)
		}
	}
}

func TestGitHubIssuesSettingRoundTripsThroughSettingsAPI(t *testing.T) {
	t.Parallel()
	settings := ReviewSettingsState{MergeMethod: "squash", ReviewLevel: "off"}
	srv := NewServer("", NewBroker())
	srv.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) { return settings, nil }
	srv.ConfigureReviewSettings = func(_ context.Context, req ReviewSettingsRequest) (ReviewSettingsState, error) {
		if req.GitHubIssuesEnabled == nil {
			return settings, errors.New("github_issues_enabled was omitted")
		}
		if req.GitHubIssueLinkMode == nil {
			return settings, errors.New("github_issue_link_mode was omitted")
		}
		if req.GitHubIssueAutoStartAssigned == nil {
			return settings, errors.New("github_issue_auto_start_assigned was omitted")
		}
		settings.GitHubIssuesEnabled = *req.GitHubIssuesEnabled
		settings.GitHubIssueLinkMode = *req.GitHubIssueLinkMode
		settings.GitHubIssueAutoStartAssigned = *req.GitHubIssueAutoStartAssigned
		return settings, nil
	}
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/review-settings", strings.NewReader(`{"auto_merge":false,"delete_merged_branches":false,"merge_method":"squash","review_level":"off","github_issues_enabled":true,"github_issue_link_mode":"close","github_issue_auto_start_assigned":true}`)))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"github_issues_enabled":true`) || !strings.Contains(response.Body.String(), `"github_issue_link_mode":"close"`) || !strings.Contains(response.Body.String(), `"github_issue_auto_start_assigned":true`) {
		t.Fatalf("POST settings response = %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/review-settings", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"github_issues_enabled":true`) || !strings.Contains(response.Body.String(), `"github_issue_link_mode":"close"`) || !strings.Contains(response.Body.String(), `"github_issue_auto_start_assigned":true`) {
		t.Fatalf("GET settings response = %d %s", response.Code, response.Body.String())
	}
}
