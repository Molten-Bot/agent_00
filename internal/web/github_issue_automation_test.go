package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

type automationGitHub struct {
	mu          sync.Mutex
	issue       GitHubIssue
	comments    string
	branch      bool
	extraLinks  bool
	unlinked    bool
	blockedBy   int
	labels      []GitHubIssueLabel
	tags        map[string]string
	refs        map[string]string
	submissions []config.Config
	fail        string
	ambiguous   bool
	calls       int
	trace       []string
}

func newAutomationGitHub() *automationGitHub {
	return &automationGitHub{issue: GitHubIssue{Number: 17, Title: "Fix backlog", Body: "Customer task", State: "OPEN", URL: "https://github.com/acme/repo/issues/17", Assignees: []GitHubIssueUser{{Login: "octocat"}}, Labels: []GitHubIssueLabel{{Name: "ready"}}}, labels: []GitHubIssueLabel{{Name: "agent_00-in-progress"}}, tags: map[string]string{}, refs: map[string]string{}}
}

func (f *automationGitHub) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.trace = append(f.trace, strings.Join(cmd.Args, " ")+" stdin="+cmd.Stdin)
	if _, ok := ctx.Deadline(); !ok {
		return execx.Result{}, errors.New("missing GitHub command deadline")
	}
	output := func(value any) (execx.Result, error) {
		body, _ := json.Marshal(value)
		return execx.Result{Stdout: string(body)}, nil
	}
	args := strings.Join(cmd.Args, " ")
	if f.fail != "" && strings.Contains(args, f.fail) {
		return execx.Result{}, errors.New("temporary GitHub failure")
	}
	if len(cmd.Args) > 1 && cmd.Args[0] == "issue" && cmd.Args[1] == "view" {
		return output(f.issue)
	}
	params := map[string]string{}
	for _, arg := range cmd.Args {
		if key, value, ok := strings.Cut(arg, "="); ok {
			params[key] = value
		}
	}
	query := params["query"]
	if strings.Contains(query, "createLinkedBranch") {
		if f.branch {
			return execx.Result{}, errors.New("Reference already exists")
		}
		f.branch = true
		if f.ambiguous {
			return execx.Result{}, errors.New("response lost after creation")
		}
		return execx.Result{Stdout: `{"data":{"createLinkedBranch":{"linkedBranch":{"ref":{"name":"agent00-issue-17"}}}}}`}, nil
	}
	if strings.Contains(query, "nodes(ids:") {
		count := 0
		if f.branch {
			count = 1
		}
		return execx.Result{Stdout: fmt.Sprintf(`{"data":{"nodes":[{"id":"I17","linkedBranches":{"totalCount":%d},"closedByPullRequestsReferences":{"totalCount":0},"issueDependenciesSummary":{"blockedBy":%d}}]}}`, count, f.blockedBy)}, nil
	}
	if strings.Contains(query, "defaultBranchRef") {
		count := 0
		nodes := "[]"
		if f.branch {
			count = 1
			nodes = `[{"ref":{"name":"agent00-issue-17"}}]`
		}
		if f.unlinked {
			count, nodes = 0, "[]"
		}
		if f.extraLinks {
			count++
		}
		return execx.Result{Stdout: fmt.Sprintf(`{"data":{"repository":{"defaultBranchRef":{"name":"main","target":{"oid":"base"}},"issue":{"id":"I17","linkedBranches":{"totalCount":%d,"nodes":%s},"closedByPullRequestsReferences":{"totalCount":0},"issueDependenciesSummary":{"blockedBy":%d}}}}}`, count, nodes, f.blockedBy)}, nil
	}
	if strings.Contains(args, "--method POST") {
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cmd.Stdin), &body); err != nil {
			return execx.Result{}, err
		}
		field := func(key string) string { var value string; _ = json.Unmarshal(body[key], &value); return value }
		switch {
		case strings.Contains(args, "/git/tags"):
			sha := fmt.Sprintf("tag-%d", len(f.tags)+1)
			f.tags[sha] = field("message")
			return output(map[string]string{"sha": sha})
		case strings.Contains(args, "/git/refs"):
			ref := field("ref")
			if _, ok := f.refs[ref]; ok {
				return execx.Result{}, errors.New("Reference already exists")
			}
			f.refs[ref] = field("sha")
			return output(map[string]string{"ref": ref})
		case strings.Contains(args, "/issues/17/labels"):
			var labels []string
			_ = json.Unmarshal(body["labels"], &labels)
			for _, label := range labels {
				f.issue.Labels = append(f.issue.Labels, GitHubIssueLabel{Name: label})
			}
			return output(f.issue.Labels)
		case strings.Contains(args, "/labels"):
			f.labels = append(f.labels, GitHubIssueLabel{Name: field("name")})
			return output(f.labels[len(f.labels)-1])
		}
	}
	endpoint := cmd.Args[len(cmd.Args)-1]
	switch {
	case endpoint == "user":
		return execx.Result{Stdout: `{"login":"octocat"}`}, nil
	case strings.Contains(endpoint, "/git/matching-refs/heads/"):
		if f.branch {
			return execx.Result{Stdout: `[{"ref":"refs/heads/agent00-issue-17"}]`}, nil
		}
		return execx.Result{Stdout: `[]`}, nil
	case strings.Contains(endpoint, "/git/matching-refs/"):
		refs := []any{}
		for ref, sha := range f.refs {
			refs = append(refs, map[string]any{"ref": ref, "object": map[string]string{"sha": sha, "type": "tag"}})
		}
		return output(refs)
	case strings.Contains(endpoint, "/git/tags/"):
		sha := endpoint[strings.LastIndex(endpoint, "/")+1:]
		return output(map[string]string{"message": f.tags[sha]})
	case strings.Contains(endpoint, "/git/ref/heads/"):
		return execx.Result{Stdout: `{"object":{"sha":"existing-commits"}}`}, nil
	case strings.Contains(endpoint, "/issues/17/comments"):
		if f.comments != "" {
			return execx.Result{Stdout: f.comments}, nil
		}
		return execx.Result{Stdout: `[]`}, nil
	case strings.Contains(endpoint, "/issues?"):
		return output([]any{map[string]any{"number": 17, "node_id": "I17", "title": f.issue.Title, "state": strings.ToLower(f.issue.State), "html_url": f.issue.URL, "assignees": f.issue.Assignees, "labels": f.issue.Labels}})
	case strings.Contains(endpoint, "/labels?"):
		return output(f.labels)
	}
	return execx.Result{}, fmt.Errorf("unexpected gh command: %s", args)
}

func (f *automationGitHub) submit(_ context.Context, body []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var run config.Config
	if err := json.Unmarshal(body, &run); err != nil {
		return "", err
	}
	f.submissions = append(f.submissions, run)
	return fmt.Sprintf("request-%d", len(f.submissions)), nil
}

func automationServer(t *testing.T, f *automationGitHub) Server {
	t.Helper()
	s := NewServer("", NewBroker())
	s.IssueClaims = &localruntime.IssueClaimStore{Root: filepath.Join(t.TempDir(), "claims")}
	s.GitHubIssueRunner = f
	s.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		return []GitHubRepo{{FullName: "acme/repo", DefaultBranch: "main"}}, nil
	}
	s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
		return ReviewSettingsState{GitHubIssuesEnabled: true, GitHubIssueAutoStartAssigned: true, GitHubIssueAutoStartOnlyAssigned: true, GitHubIssueAutoStartLabels: []string{"ready"}}, nil
	}
	s.SubmitLocalPrompt = f.submit
	return s
}

func TestIssueAutomationBacklogRestartAndMetadata(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	reconcileOrAdmitTestIssue(context.Background(), s)
	if len(f.submissions) != 1 {
		t.Fatalf("submissions=%d status=%+v trace=%v", len(f.submissions), s.issueAutomation.status, f.trace)
	}
	run := f.submissions[0]
	if run.GitHubIssue.Claim == nil || run.GitHubIssue.Claim.Branch != "agent00-issue-17" || run.BaseBranch != "main" || !strings.Contains(run.Prompt, "Customer task") {
		t.Fatalf("missing claim/context: %+v", run)
	}
	reconcileOrAdmitTestIssue(context.Background(), s)
	restarted := automationServer(t, f)
	restarted.IssueClaims = s.IssueClaims
	reconcileOrAdmitTestIssue(context.Background(), restarted)
	if len(f.submissions) != 1 {
		t.Fatal("restart or repeated poll resubmitted")
	}
	records, err := s.IssueClaims.List()
	if err != nil || len(records) != 1 || records[0].RequestID != "request-1" {
		t.Fatalf("claim receipt=%+v err=%v", records, err)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/github/automation", nil))
	if strings.Contains(response.Body.String(), "Customer task") {
		t.Fatal("status exposed stored prompt")
	}
}

func TestIssueAutomationExplicitRetryOverridesOpenDependency(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	reconcileOrAdmitTestIssue(context.Background(), s)
	claimID := f.submissions[0].GitHubIssue.Claim.ID
	f.blockedBy = 1
	response := retryIssue(t, s, claimID, true)
	if response.Code != http.StatusAccepted {
		t.Fatalf("blocked retry = %d %s", response.Code, response.Body.String())
	}
	if len(f.submissions) != 2 {
		t.Fatalf("blocked retry mutated claim: submissions=%d tags=%d", len(f.submissions), len(f.tags))
	}
}

func TestIssueAutomationIndependentInstancesClaimOnce(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	a, b := automationServer(t, f), automationServer(t, f)
	var wg sync.WaitGroup
	for _, s := range []Server{a, b} {
		wg.Go(func() { reconcileOrAdmitTestIssue(context.Background(), s) })
	}
	wg.Wait()
	if len(f.submissions) != 1 {
		t.Fatalf("concurrent instances submitted %d tasks", len(f.submissions))
	}
}

func TestIssueAutomationFailuresRemainVisibleAndRecoverable(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"settings", "graphql", "/labels?", "submit", "ambiguous"} {
		t.Run(stage, func(t *testing.T) {
			f := newAutomationGitHub()
			s := automationServer(t, f)
			switch stage {
			case "settings":
				s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
					return ReviewSettingsState{}, errors.New("corrupt settings")
				}
			case "submit":
				s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) { return "", errors.New("agent authorization required") }
			case "ambiguous":
				f.ambiguous = true
			default:
				f.fail = stage
			}
			reconcileOrAdmitTestIssue(context.Background(), s)
			if s.issueAutomation.status.Error == "" {
				t.Fatal("failure hidden")
			}
			if len(f.submissions) != 0 {
				t.Fatal("failed admission submitted")
			}
			if f.branch {
				f.fail = ""
				f.ambiguous = false
				s.SubmitLocalPrompt = f.submit
				reconcileOrAdmitTestIssue(context.Background(), s)
				if len(f.submissions) != 0 {
					t.Fatal("partial claim was automatically replayed")
				}
				meta, err := s.latestIssueAttempt(context.Background(), "acme/repo", 17)
				if err != nil {
					t.Fatal(err)
				}
				expected := meta.Claim.ID
				if expected == "" {
					expected = "unrecorded"
				}
				response := retryIssue(t, s, expected, true)
				if response.Code != http.StatusAccepted {
					t.Fatalf("recovery: %d %s", response.Code, response.Body.String())
				}
				if len(f.submissions) != 1 || f.submissions[0].GitHubIssue.Claim.Branch != "agent00-issue-17" {
					t.Fatal("recovery did not preserve branch")
				}
			}
		})
	}
}

func retryIssue(t *testing.T, s Server, expected string, confirmed bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"repository": "acme/repo", "number": 17, "expectedClaimId": expected, "confirmStopped": confirmed})
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/api/github/automation/retry", strings.NewReader(string(body))))
	return response
}

func TestIssueAutomationRetryRequiresConfirmationAndSerializesInstances(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	initial := automationServer(t, f)
	reconcileOrAdmitTestIssue(context.Background(), initial)
	id := f.submissions[0].GitHubIssue.Claim.ID
	if response := retryIssue(t, initial, id, false); response.Code != 409 {
		t.Fatalf("unconfirmed recovery=%d", response.Code)
	}
	a, b := automationServer(t, f), automationServer(t, f)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, s := range []Server{a, b} {
		wg.Go(func() { codes <- retryIssue(t, s, id, true).Code })
	}
	wg.Wait()
	close(codes)
	accepted := 0
	for code := range codes {
		if code == 202 {
			accepted++
		} else if code != 409 {
			t.Fatalf("unexpected recovery status %d", code)
		}
	}
	if accepted != 1 || len(f.submissions) != 2 {
		t.Fatalf("accepted retries=%d submissions=%d", accepted, len(f.submissions))
	}
	if response := retryIssue(t, initial, id, true); response.Code != 409 {
		t.Fatal("stale recovery accepted")
	}
}

func TestIssueAutomationManualStartsShareClaims(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	settings, _ := s.ReviewSettingsStatus(context.Background())
	settings.GitHubIssueRepositories = localruntime.GitHubIssueRepositories{}
	s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) { return settings, nil }
	f.issue.Assignees = nil
	f.issue.Labels = nil
	run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, f.issue, "reference")
	body, _ := json.Marshal(run)
	// The parser also accepts an encoded JSON string. It must not bypass claims.
	encoded, _ := json.Marshal(string(body))
	if _, err := s.submitWithIssueClaim(context.Background(), encoded, f.submit); err != nil {
		t.Fatal(err)
	}
	if len(f.submissions) != 1 || f.submissions[0].GitHubIssue.Claim == nil {
		t.Fatal("manual start bypassed ownership")
	}
	saved, _ := json.Marshal(f.submissions[0])
	s.Broker.RecordTaskRunConfig("request-1", saved)
	if _, err := s.submitWithIssueClaim(context.Background(), body, f.submit); err == nil {
		t.Fatal("manual duplicate accepted")
	}
	forged, _ := json.Marshal(f.submissions[0])
	if _, err := s.submitWithIssueClaim(context.Background(), forged, f.submit); err == nil {
		t.Fatal("client-supplied claim bypassed recovery")
	}
}

func TestIssueAutomationRefusesActiveLocalRetry(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	reconcileOrAdmitTestIssue(context.Background(), s)
	body, _ := json.Marshal(f.submissions[0])
	s.Broker.RecordTaskRunConfig("request-1", body)
	response := retryIssue(t, s, f.submissions[0].GitHubIssue.Claim.ID, true)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "active task") {
		t.Fatalf("active retry=%d %s", response.Code, response.Body.String())
	}
}

func TestIssueAutomationPersistsBeforeGitHubMutation(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	s.IssueClaims.Root = root
	reconcileOrAdmitTestIssue(context.Background(), s)
	if f.branch || len(f.submissions) > 0 || s.issueAutomation.status.Error == "" {
		t.Fatalf("storage failure mutated GitHub: %+v", s.issueAutomation.status)
	}
}

func TestIssueAutomationGitHubDeadlineAndPartialBranchData(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		if strings.Contains(strings.Join(cmd.Args, " "), "defaultBranchRef") {
			return execx.Result{Stdout: `{"data":{}}`}, nil
		}
		return f.Run(ctx, cmd)
	})
	reconcileOrAdmitTestIssue(context.Background(), s)
	if f.branch || s.issueAutomation.status.Error == "" {
		t.Fatal("incomplete associations treated as no branches")
	}
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, _ execx.Command) (execx.Result, error) {
		<-ctx.Done()
		return execx.Result{}, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := s.issueGitHubLogin(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline ignored: %v", err)
	}
}

// Seed a claim with an explicit item, then reconcile that durable claim on later calls.
func reconcileOrAdmitTestIssue(ctx context.Context, s Server) {
	err := s.ReconcileIssueWork(ctx)
	records, listErr := s.IssueClaims.List()
	if err == nil && len(records) == 0 && (s.IssueRecovery != nil || listErr == nil) {
		var settings ReviewSettingsState
		settings, err = s.ReviewSettingsStatus(ctx)
		if err == nil {
			var login string
			login, err = s.issueGitHubLogin(ctx)
			if err == nil {
				run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, GitHubIssue{Number: 17}, settings.GitHubIssueLinkMode)
				if s.Workflows != nil {
					run.Workflow = &config.WorkflowRunConfig{ID: "issues", EventID: "issue-17"}
					settings, _, err = s.workflowIssueSettings(run)
					if err != nil {
						return
					}
				}
				_, err = s.claimAndSubmitIssue(ctx, run, settings, login, s.SubmitLocalPrompt)
			}
		}
	}
	var status issueAutomationStatus
	if err = errors.Join(err, listErr); err != nil {
		status.addError(err)
	}
	s.issueAutomation.mu.Lock()
	s.issueAutomation.status = status
	s.issueAutomation.mu.Unlock()
}
