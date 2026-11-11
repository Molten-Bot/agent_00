package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestGitHubLabelsListsUniqueLabelsAcrossAccessibleRepositories(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if !reflect.DeepEqual(cmd.Args[:2], []string{"label", "list"}) {
			t.Fatalf("command = %v, want label list", cmd.Args)
		}
		if strings.Contains(strings.Join(cmd.Args, " "), "acme/one") {
			return execx.Result{Stdout: `[{"name":"bug"},{"name":"priority"}]`}, nil
		}
		return execx.Result{Stdout: `[{"name":"priority"},{"name":"feature"}]`}, nil
	}), true)
	srv.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		return []GitHubRepo{{FullName: "acme/one"}, {FullName: "acme/two"}}, nil
	}
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/labels", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"labels":["bug","feature","priority"]`) {
		t.Fatalf("labels response = %d %s", response.Code, response.Body.String())
	}
}

func TestGitHubLabelsReportsFailedRepositoryQuery(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if strings.Contains(strings.Join(cmd.Args, " "), "acme/failing") {
			return execx.Result{}, errors.New("GitHub API unavailable")
		}
		return execx.Result{Stdout: `[{"name":"bug"}]`}, nil
	}), true)
	srv.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		return []GitHubRepo{{FullName: "acme/working"}, {FullName: "acme/failing"}}, nil
	}

	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/labels", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"ok":false`) || !strings.Contains(response.Body.String(), "list GitHub labels for acme/failing: GitHub API unavailable") {
		t.Fatalf("response = %s, want failed repository query error", response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"labels"`) {
		t.Fatalf("response = %s, must not return partial labels", response.Body.String())
	}
}

func TestGitHubIssueAutoStartRequiresEverySelectedLabel(t *testing.T) {
	t.Parallel()
	issue := GitHubIssue{Labels: []GitHubIssueLabel{{Name: "bug"}, {Name: "priority"}}}
	if !githubIssueHasAllLabels(issue, []string{"BUG", "priority"}) {
		t.Fatal("matching labels should pass case-insensitively")
	}
	if githubIssueHasAllLabels(issue, []string{"bug", "feature"}) {
		t.Fatal("issue missing one required label should not pass")
	}
}

// Searching is scoped to the assignee filter in the browser, and that filter is on by
// default, so the combined query is the common path rather than an edge case.
func TestGitHubIssueListSearchesWithinIssuesAssignedToMe(t *testing.T) {
	t.Parallel()
	var got execx.Command
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		got = cmd
		return execx.Result{Stdout: `[]`}, nil
	}), true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo&search=browser&assignee=me", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	want := []string{"issue", "list", "--repo", "acme/repo", "--state", "open", "--limit", "100", "--json", "number,title,state,url,author,labels,assignees,createdAt,updatedAt", "--search", "browser", "--assignee", "@me"}
	if got.Name != "gh" || !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("command = %s %v, want gh %v", got.Name, got.Args, want)
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
	if err := srv.scanGitHubIssuesForAutoStart(context.Background(), true, seen, config.GitHubIssueLinkClose, true, nil); err != nil {
		t.Fatalf("baseline scan error: %v", err)
	}
	if len(submitted) != 0 || !seen["acme/repo#22"] {
		t.Fatalf("baseline submitted %d tasks, seen=%v", len(submitted), seen)
	}
	assigned = true
	if err := srv.scanGitHubIssuesForAutoStart(context.Background(), false, seen, config.GitHubIssueLinkClose, true, nil); err != nil {
		t.Fatalf("new issue scan error: %v", err)
	}
	if err := srv.scanGitHubIssuesForAutoStart(context.Background(), false, seen, config.GitHubIssueLinkClose, true, nil); err != nil {
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

func TestGitHubIssueAutoStartBaselinesWhenFiltersChange(t *testing.T) {
	t.Parallel()
	var submitted [][]byte
	labels := []string{"bug"}
	srv := NewServer("", NewBroker())
	srv.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
		return ReviewSettingsState{
			GitHubIssuesEnabled:              true,
			GitHubIssueAutoStartAssigned:     true,
			GitHubIssueAutoStartOnlyAssigned: false,
			GitHubIssueAutoStartLabels:       labels,
		}, nil
	}
	srv.GitHubIssueRunner = githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if len(cmd.Args) >= 2 && cmd.Args[0] == "issue" && cmd.Args[1] == "list" {
			return execx.Result{Stdout: `[{"number":22,"title":"Existing bug","state":"OPEN","labels":[{"name":"bug"}]},{"number":23,"title":"Existing feature","state":"OPEN","labels":[{"name":"feature"}]}]`}, nil
		}
		t.Fatalf("unexpected gh command: %#v", cmd)
		return execx.Result{}, nil
	})
	srv.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		return []GitHubRepo{{FullName: "acme/repo"}}, nil
	}
	srv.SubmitLocalPrompt = func(_ context.Context, body []byte) (string, error) {
		submitted = append(submitted, append([]byte(nil), body...))
		return "request-1", nil
	}
	seen := make(map[string]bool)
	baseline := true
	filters := ""
	srv.pollGitHubIssueAutoStart(context.Background(), seen, &baseline, &filters)
	labels = []string{"feature"}
	srv.pollGitHubIssueAutoStart(context.Background(), seen, &baseline, &filters)
	if len(submitted) != 0 {
		t.Fatalf("filter change submitted %d tasks, want 0", len(submitted))
	}
	if !seen["acme/repo#22"] || !seen["acme/repo#23"] {
		t.Fatalf("filter baseline missed existing issues: %v", seen)
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
		"/api/github/issues?repo=acme/repo&branch=sometimes",
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

func TestGitHubIssueListFiltersByLinkedBranch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		branch     string
		wantNumber string
		omitNumber string
	}{
		{branch: "with", wantNumber: `"number":17`, omitNumber: `"number":18`},
		{branch: "without", wantNumber: `"number":18`, omitNumber: `"number":17`},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			t.Parallel()
			var commands []execx.Command
			srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				commands = append(commands, cmd)
				if len(cmd.Args) > 1 && cmd.Args[0] == "issue" && cmd.Args[1] == "list" {
					return execx.Result{Stdout: `[{"id":"I_17","number":17,"title":"Started"},{"id":"I_18","number":18,"title":"Unstarted"}]`}, nil
				}
				return execx.Result{Stdout: `{"data":{"nodes":[{"id":"I_17","linkedBranches":{"totalCount":1}},{"id":"I_18","linkedBranches":{"totalCount":0}}]}}`}, nil
			}), true)
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo&branch="+tc.branch, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
			}
			if len(commands) != 2 || !strings.Contains(strings.Join(commands[0].Args, " "), "--json id,number") || !strings.Contains(strings.Join(commands[1].Args, " "), "linkedBranches(first:1)") || !reflect.DeepEqual(commands[1].Args[len(commands[1].Args)-4:], []string{"-F", "ids[]=I_17", "-F", "ids[]=I_18"}) {
				t.Fatalf("commands = %#v, want issue IDs followed by batched linked-branch query", commands)
			}
			if !strings.Contains(response.Body.String(), tc.wantNumber) || strings.Contains(response.Body.String(), tc.omitNumber) {
				t.Fatalf("response = %s, want %s and no %s", response.Body.String(), tc.wantNumber, tc.omitNumber)
			}
		})
	}
}

func TestGitHubIssueListBranchFilterChecksCandidatesPastResponseLimit(t *testing.T) {
	t.Parallel()
	entries := make([]githubIssueListEntry, 1001)
	for index := range entries {
		entries[index] = githubIssueListEntry{
			GitHubIssue: GitHubIssue{Number: index + 1, Title: "Issue"},
			ID:          "I_" + strconv.Itoa(index+1),
		}
	}
	var listLimits []int
	graphQLCalls := 0
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if len(cmd.Args) > 1 && cmd.Args[0] == "issue" && cmd.Args[1] == "list" {
			for index, arg := range cmd.Args {
				if arg == "--limit" && index+1 < len(cmd.Args) {
					limit, parseErr := strconv.Atoi(cmd.Args[index+1])
					if parseErr != nil {
						return execx.Result{}, parseErr
					}
					listLimits = append(listLimits, limit)
					end := min(limit, len(entries))
					encodedEntries, marshalErr := json.Marshal(entries[:end])
					return execx.Result{Stdout: string(encodedEntries)}, marshalErr
				}
			}
			return execx.Result{}, errors.New("issue list command has no limit")
		}
		graphQLCalls++
		response := githubLinkedBranchResponse{}
		for _, arg := range cmd.Args {
			id := strings.TrimPrefix(arg, "ids[]=")
			if id == arg {
				continue
			}
			node := struct {
				ID             string `json:"id"`
				LinkedBranches struct {
					TotalCount int `json:"totalCount"`
				} `json:"linkedBranches"`
			}{ID: id}
			if id == "I_1001" {
				node.LinkedBranches.TotalCount = 1
			}
			response.Data.Nodes = append(response.Data.Nodes, node)
		}
		encodedResponse, marshalErr := json.Marshal(response)
		return execx.Result{Stdout: string(encodedResponse)}, marshalErr
	}), true)

	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo&branch=with", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !reflect.DeepEqual(listLimits, []int{100, 200, 400, 800, 1600}) {
		t.Fatalf("branch-filter candidate limits = %v, want progressively expanded search beyond 1000", listLimits)
	}
	if graphQLCalls != 11 {
		t.Fatalf("linked-branch query count = %d, want 11 cached batches", graphQLCalls)
	}
	if !strings.Contains(response.Body.String(), `"number":1001`) || strings.Contains(response.Body.String(), `"number":1,`) {
		t.Fatalf("response = %s, want matching issue beyond former candidate limit only", response.Body.String())
	}
}

func TestGitHubIssueListBranchFilterReportsCappedSearch(t *testing.T) {
	t.Parallel()
	entries := make([]githubIssueListEntry, githubIssueSearchLimit+1)
	for index := range entries {
		entries[index] = githubIssueListEntry{
			GitHubIssue: GitHubIssue{Number: index + 1, Title: "Issue"},
			ID:          "I_" + strconv.Itoa(index+1),
		}
	}
	var listLimits []int
	graphQLCalls := 0
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if len(cmd.Args) > 1 && cmd.Args[0] == "issue" && cmd.Args[1] == "list" {
			if !slices.Contains(cmd.Args, "--search") {
				return execx.Result{}, errors.New("issue list command has no search")
			}
			for index, arg := range cmd.Args {
				if arg == "--limit" && index+1 < len(cmd.Args) {
					limit, parseErr := strconv.Atoi(cmd.Args[index+1])
					if parseErr != nil {
						return execx.Result{}, parseErr
					}
					listLimits = append(listLimits, limit)
					end := min(limit, githubIssueSearchLimit)
					encodedEntries, marshalErr := json.Marshal(entries[:end])
					return execx.Result{Stdout: string(encodedEntries)}, marshalErr
				}
			}
			return execx.Result{}, errors.New("issue list command has no limit")
		}
		if slices.Contains(cmd.Args, "search/issues") {
			return execx.Result{Stdout: `{"total_count":1001,"incomplete_results":false}`}, nil
		}
		graphQLCalls++
		response := githubLinkedBranchResponse{}
		for _, arg := range cmd.Args {
			id := strings.TrimPrefix(arg, "ids[]=")
			if id == arg {
				continue
			}
			node := struct {
				ID             string `json:"id"`
				LinkedBranches struct {
					TotalCount int `json:"totalCount"`
				} `json:"linkedBranches"`
			}{ID: id}
			if id == "I_1001" {
				node.LinkedBranches.TotalCount = 1
			}
			response.Data.Nodes = append(response.Data.Nodes, node)
		}
		encodedResponse, marshalErr := json.Marshal(response)
		return execx.Result{Stdout: string(encodedResponse)}, marshalErr
	}), true)

	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo&branch=with&search=issue", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", response.Code, response.Body.String())
	}
	if !reflect.DeepEqual(listLimits, []int{100, 200, 400, 800, 1000}) {
		t.Fatalf("branch-filter search limits = %v, want expansion through GitHub's cap", listLimits)
	}
	if graphQLCalls != 10 {
		t.Fatalf("linked-branch query count = %d, want 10 cached batches", graphQLCalls)
	}
	if !strings.Contains(response.Body.String(), "narrow the search") {
		t.Fatalf("response = %s, want capped-search guidance", response.Body.String())
	}
}

func TestGitHubIssueListBranchFilterAllowsExactSearchMaximum(t *testing.T) {
	t.Parallel()
	entries := make([]githubIssueListEntry, githubIssueSearchLimit)
	for index := range entries {
		entries[index] = githubIssueListEntry{
			GitHubIssue: GitHubIssue{Number: index + 1, Title: "Issue"},
			ID:          "I_" + strconv.Itoa(index+1),
		}
	}
	var countArgs []string
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if len(cmd.Args) > 1 && cmd.Args[0] == "issue" && cmd.Args[1] == "list" {
			limit, parseErr := strconv.Atoi(cmd.Args[slices.Index(cmd.Args, "--limit")+1])
			if parseErr != nil {
				return execx.Result{}, parseErr
			}
			encodedEntries, marshalErr := json.Marshal(entries[:min(limit, len(entries))])
			return execx.Result{Stdout: string(encodedEntries)}, marshalErr
		}
		if slices.Contains(cmd.Args, "search/issues") {
			countArgs = cmd.Args
			return execx.Result{Stdout: `{"total_count":1000,"incomplete_results":false}`}, nil
		}
		response := githubLinkedBranchResponse{}
		for _, arg := range cmd.Args {
			id := strings.TrimPrefix(arg, "ids[]=")
			if id != arg {
				response.Data.Nodes = append(response.Data.Nodes, struct {
					ID             string `json:"id"`
					LinkedBranches struct {
						TotalCount int `json:"totalCount"`
					} `json:"linkedBranches"`
				}{ID: id})
			}
		}
		encodedResponse, marshalErr := json.Marshal(response)
		return execx.Result{Stdout: string(encodedResponse)}, marshalErr
	}), true)

	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues?repo=acme/repo&branch=with&search=issue&assignee=me", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !slices.Contains(countArgs, "q=issue repo:acme/repo is:issue state:open assignee:@me") {
		t.Fatalf("count args = %v, want equivalent search query", countArgs)
	}
	if !strings.Contains(response.Body.String(), `"issues":[]`) {
		t.Fatalf("response = %s, want successful empty filtered result", response.Body.String())
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
	for _, want := range []string{"GitHub Issues", "agent-00-header", "github-issues-dock-link", `href="#studio-issues"`, `data-prompt-title-mode="issues"`, `id="github-issues-panel"`, "github-issues-repo", "github-issues-search", "github-issues-branch-filter", `data-lucide="git-branch"`, `data-lucide="git-branch-plus"`, `data-lucide="git-branch-minus"`, `Issues (<span id="github-issues-count">0</span>):`, "github-issues-assignee-filter", "user-round-arrow-left", "review-settings-github-issues-link-mode", "review-settings-github-issues-link-close", `href="/static/style.css?v=6"`, `src="/static/github-issues.js?v=18"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	branchFilterAt := strings.Index(page, `id="github-issues-branch-filter"`)
	assigneeFilterAt := strings.Index(page, `id="github-issues-assignee-filter"`)
	if branchFilterAt < 0 || assigneeFilterAt < branchFilterAt {
		t.Error("branch icon filters should sit directly before the assigned-to-me filter")
	}
	if strings.Contains(page, `<select id="github-issues-branch-filter"`) {
		t.Error("branch filter should use icon buttons, not a dropdown")
	}
	branchFilterMarkup := page[branchFilterAt:assigneeFilterAt]
	if count := strings.Count(branchFilterMarkup, `</button>`); count != 1 {
		t.Errorf("branch filter renders %d buttons, want one cycling button", count)
	}
	for _, value := range []string{"all", "with", "without"} {
		if !strings.Contains(branchFilterMarkup, `data-branch-filter-icon="`+value+`"`) {
			t.Errorf("branch filter is missing %q cycle icon", value)
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
	if asset.Code != http.StatusOK || strings.Contains(asset.Body.String(), "Edit prompt first") || strings.Contains(asset.Body.String(), "Start with this prompt") || !strings.Contains(asset.Body.String(), "Start task") || !strings.Contains(asset.Body.String(), `state: "open"`) || !strings.Contains(asset.Body.String(), "open issues") || !strings.Contains(asset.Body.String(), "linkMode: githubIssueLinkMode") || !strings.Contains(asset.Body.String(), "githubIssue:") || !strings.Contains(asset.Body.String(), "loadingIssueNumber") || !strings.Contains(asset.Body.String(), "task-icons-breathe") || !strings.Contains(asset.Body.String(), `"X-Agent-Task-Source": "github_issues"`) {
		t.Fatalf("issue browser asset response = %d, expected issue-only task launch", asset.Code)
	}
}

// TestGitHubIssueHeaderAssetsCarryAssigneeCopy keeps the header's user-visible
// copy from disappearing in environments without Node, where the browser suite
// below is skipped. It asserts the strings a reader sees, not the source that
// produces them, so a refactor of the rendering code stays free.
func TestGitHubIssueHeaderAssetsCarryAssigneeCopy(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	githubIssueTestServer(nil, true).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/static/github-issues.js", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /static/github-issues.js status = %d, want 200", response.Code)
	}
	script := response.Body.String()
	for _, want := range []string{"Assignees", "Unassigned", "Created", "Updated"} {
		if !strings.Contains(script, want) {
			t.Errorf("github-issues.js never writes %q, so the issue header lost it", want)
		}
	}
}

// TestGitHubIssueHeaderRendersInBrowser drives the real static/github-issues.js
// in Chromium. renderIssueDetail, addAssignees, and addTimestamp are unexported
// closures with no Go-reachable equivalent, so the header's placement, the
// assignee icons and their hover/focus-revealed names, the Unassigned fallback,
// and the viewer-local timestamps are only observable through the rendered DOM.
func TestGitHubIssueHeaderRendersInBrowser(t *testing.T) {
	requireBrowserTooling(t, "GitHub issue header check")

	ts := httptest.NewServer(githubIssueTestServer(nil, true).Handler())
	defer ts.Close()

	// Same budget as the settings-modal suite, the other multi-case Chromium
	// run in this package: a dozen cases each launching a page is well past
	// what a one-case suite's 60s is sized for.
	runBrowserSuite(t, ts.URL, 3*time.Minute, "GitHub issue header checks", "tests/github_issues.cjs")
}

// openingTagWithID returns the opening tag that carries id="<id>" so assertions can check
// attributes individually instead of pinning the order the markup happens to list them in.
func openingTagWithID(page, id string) (string, bool) {
	at := strings.Index(page, `id="`+id+`"`)
	if at < 0 {
		return "", false
	}
	start := strings.LastIndex(page[:at], "<")
	end := strings.Index(page[at:], ">")
	if start < 0 || end < 0 {
		return "", false
	}
	return page[start : at+end+1], true
}

func TestGitHubIssueBrowserDefaultsToIssuesAssignedToMe(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(nil, true)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	page := response.Body.String()
	filterTag, ok := openingTagWithID(page, "github-issues-assignee-filter")
	if !ok {
		t.Fatal("page is missing the assignee filter button")
	}
	for _, want := range []string{`class="github-issues-assignee-filter"`, `type="button"`, `aria-pressed="true"`, `aria-label="Show only issues assigned to me"`} {
		if !strings.Contains(filterTag, want) {
			t.Errorf("assignee filter tag %q is missing %q, so assigned issues do not show pressed by default", filterTag, want)
		}
	}
	if strings.Contains(page, `aria-label="Show all issues"`) {
		t.Error("the toggle name should describe the filter, not contradict aria-pressed")
	}
	if !strings.Contains(page, `placeholder="Search issues assigned to you"`) {
		t.Error("search placeholder should name the assignee scope it searches by default")
	}
	// The browser-side half of this default — the assignee query parameter, the count and
	// empty-state wording, and remembering the filter choice — is asserted through the real UI
	// in internal/web/tests/settings_tabs.cjs rather than by matching github-issues.js source.
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
		if req.AssignCreator == nil {
			return settings, errors.New("assign_creator was omitted")
		}
		settings.GitHubIssuesEnabled = *req.GitHubIssuesEnabled
		settings.GitHubIssueLinkMode = *req.GitHubIssueLinkMode
		settings.GitHubIssueAutoStartAssigned = *req.GitHubIssueAutoStartAssigned
		settings.AssignCreator = *req.AssignCreator
		return settings, nil
	}
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/review-settings", strings.NewReader(`{"auto_merge":false,"delete_merged_branches":false,"merge_method":"squash","review_level":"off","github_issues_enabled":true,"github_issue_link_mode":"close","github_issue_auto_start_assigned":true,"assign_creator":false}`)))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"github_issues_enabled":true`) || !strings.Contains(response.Body.String(), `"github_issue_link_mode":"close"`) || !strings.Contains(response.Body.String(), `"github_issue_auto_start_assigned":true`) || !strings.Contains(response.Body.String(), `"assign_creator":false`) {
		t.Fatalf("POST settings response = %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/review-settings", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"github_issues_enabled":true`) || !strings.Contains(response.Body.String(), `"github_issue_link_mode":"close"`) || !strings.Contains(response.Body.String(), `"github_issue_auto_start_assigned":true`) || !strings.Contains(response.Body.String(), `"assign_creator":false`) {
		t.Fatalf("GET settings response = %d %s", response.Code, response.Body.String())
	}
}
