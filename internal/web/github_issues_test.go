package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
	for _, want := range []string{"GitHub Issues", "agent-00-header", "github-issues-dock-link", `href="#studio-issues"`, `data-prompt-title-mode="issues"`, `id="github-issues-panel"`, "github-issues-repo", "github-issues-state", "github-issues-search", "github-issues-count", "review-settings-github-issues-link-mode", "review-settings-github-issues-link-close", "github-issues.css", "github-issues.js"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
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
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "Edit prompt first") || !strings.Contains(asset.Body.String(), "Start task") || !strings.Contains(asset.Body.String(), "Start with this prompt") || !strings.Contains(asset.Body.String(), "linkMode: githubIssueLinkMode") || !strings.Contains(asset.Body.String(), "githubIssue:") {
		t.Fatalf("issue browser asset response = %d, expected both task launch modes", asset.Code)
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
		settings.GitHubIssuesEnabled = *req.GitHubIssuesEnabled
		settings.GitHubIssueLinkMode = *req.GitHubIssueLinkMode
		return settings, nil
	}
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/review-settings", strings.NewReader(`{"auto_merge":false,"delete_merged_branches":false,"merge_method":"squash","review_level":"off","github_issues_enabled":true,"github_issue_link_mode":"close"}`)))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"github_issues_enabled":true`) || !strings.Contains(response.Body.String(), `"github_issue_link_mode":"close"`) {
		t.Fatalf("POST settings response = %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/review-settings", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"github_issues_enabled":true`) || !strings.Contains(response.Body.String(), `"github_issue_link_mode":"close"`) {
		t.Fatalf("GET settings response = %d %s", response.Code, response.Body.String())
	}
}
