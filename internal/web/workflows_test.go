package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/library"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestWorkflowScanAPIWakesPoller(t *testing.T) {
	t.Parallel()
	srv := NewServer("", NewBroker())
	h := srv.Handler()
	request := func(method, origin string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://localhost/api/workflows/scan", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(http.MethodPost, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("scan with no engine returned %d", rec.Code)
	}
	scans := make(chan struct{}, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Workflows = &workflow.Engine{
		Store:     &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")},
		StatePath: filepath.Join(t.TempDir(), "workflow-state.json"),
		Enabled: func() bool {
			scans <- struct{}{}
			return false
		},
	}
	h = srv.Handler()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Workflows.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	awaitScan := func() {
		t.Helper()
		select {
		case <-scans:
		case <-ctx.Done():
			t.Fatal("workflow scan did not run")
		}
	}
	awaitScan()
	if rec := request(http.MethodGet, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET scan returned %d", rec.Code)
	}
	if rec := request(http.MethodPost, "https://example.com"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin scan returned %d", rec.Code)
	}
	if rec := request(http.MethodPost, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("scan returned %d: %s", rec.Code, rec.Body)
	}
	awaitScan()
}

func TestWorkflowAPIStoreAndPreview(t *testing.T) {
	t.Parallel()
	srv := NewServer("", NewBroker())
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	calls := 0
	srv.Workflows = &workflow.Engine{Store: store, Allowed: func(repo string) bool { return repo != "acme/disabled" }, Fetch: func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) {
		calls++
		return []workflow.Item{{Number: 1, Kind: "issue", Repository: "acme/repo", Open: true}}, nil
	}}
	h := srv.Handler()
	request := func(method, path string, data any) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(data)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(body)))
		return rec
	}
	if rec := request(http.MethodGet, "/api/workflows/runs", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("activation history endpoint returned %d", rec.Code)
	}
	rec := request("GET", "/api/workflows", nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	var d workflow.Document
	json.Unmarshal(rec.Body.Bytes(), &d)
	w := workflow.Definition{ID: "one", Name: "One", Repository: "acme/repo", Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all", Rules: []workflow.Rule{}}, Task: workflow.Task{Kind: "implement", Instructions: "Implement", IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash"}}
	d.Workflows = append(d.Workflows, w)
	rec = request("PUT", "/api/workflows", d)
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if stale := request("PUT", "/api/workflows", d); stale.Code != 409 {
		t.Fatalf("stale write %d", stale.Code)
	}
	rec = request("POST", "/api/workflows/preview", w)
	if rec.Code != 200 || calls != 1 {
		t.Fatal(rec.Body, calls)
	}
	var decisions []workflow.Decision
	json.Unmarshal(rec.Body.Bytes(), &decisions)
	if len(decisions) != 1 || !decisions[0].Matches {
		t.Fatal(decisions)
	}
	for _, suffix := range []string{" {}", " null", " trailing", strings.Repeat(" ", 1<<20)} {
		body, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/workflows/preview", strings.NewReader(string(body)+suffix)))
		if rec.Code != http.StatusBadRequest || calls != 1 {
			t.Fatalf("invalid preview body reached repository fetch: status=%d calls=%d", rec.Code, calls)
		}
	}
	w.Repository = "acme/disabled"
	rec = request("POST", "/api/workflows/preview", w)
	if rec.Code != 400 || calls != 1 {
		t.Fatal("preview bypassed repo gate")
	}
	d.Version = 99
	if rec = request("PUT", "/api/workflows", d); rec.Code != 400 {
		t.Fatal(rec.Body)
	}
}

func TestWorkflowIssueRetainsDurableClaimAndFreshPrompt(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	f.comments = issuePromptCommentPages
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	w := workflow.Definition{ID: "issues", Name: "Implement", Repository: "acme/repo", Enabled: true, Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", Instructions: "Add a regression test", IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash"}}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	s.Workflows = &workflow.Engine{Store: store, Enabled: func() bool { return true }, Allowed: func(string) bool { return true }}
	run := issueRunConfig(GitHubRepo{FullName: "acme/repo"}, f.issue, config.GitHubIssueLinkReference)
	run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: "activation", Name: w.Name, Instructions: w.Task.Instructions}
	run.Prompt = "Stale issue data"
	if _, err := s.SubmitWorkflowRun(context.Background(), run, f.submit); err != nil {
		t.Fatal(err)
	}
	if len(f.submissions) != 1 {
		t.Fatal("workflow did not submit")
	}
	got := f.submissions[0]
	if got.GitHubIssue.Claim == nil || got.GitHubIssue.Claim.Branch != "agent00-issue-17" || got.Workflow.EventID != "activation" {
		t.Fatalf("lost durable ownership or workflow identity: %+v", got)
	}
	if !strings.Contains(got.Prompt, "Add a regression test") || !strings.Contains(got.Prompt, "First page requirement") || strings.Contains(got.Prompt, "Stale issue data") {
		t.Fatal("workflow instructions or fresh issue evidence missing")
	}
	record := latestRecoveryRecord(t, s)
	if record.Version != 2 {
		t.Fatal("workflow bypassed durable admission")
	}
	if _, err := s.SubmitWorkflowRun(context.Background(), run, f.submit); err == nil {
		t.Fatal("duplicate issue claim admitted")
	}
}

func TestWorkflowLibraryEnableValidationAndUnavailableScheduler(t *testing.T) {
	t.Parallel()
	s := NewServer("", NewBroker())
	w := workflow.Definition{ID: "scheduled", Name: "Scheduled", Enabled: true, Repository: "acme/repo", Trigger: workflow.Trigger{Event: "schedule", IntervalHours: 24}, Task: workflow.Task{LibraryTaskName: "custom-audit"}}
	doc := workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}
	s.LoadLibraryTasks = func() ([]library.TaskSummary, error) {
		return []library.TaskSummary{{Name: "custom-audit", PromptContextRequired: true, RequiresNonDefaultBranch: true}}, nil
	}
	if s.validateWorkflowLibrary(doc) == nil {
		t.Fatal("missing required context accepted")
	}
	doc.Workflows[0].Task.Instructions = "Audit subsystem"
	if s.validateWorkflowLibrary(doc) == nil {
		t.Fatal("missing required branch accepted")
	}
	doc.Workflows[0].Task.BaseBranch = "feature"
	if err := s.validateWorkflowLibrary(doc); err != nil {
		t.Fatal(err)
	}
	doc.Workflows[0].Task.LibraryTaskName = "missing"
	if s.validateWorkflowLibrary(doc) == nil {
		t.Fatal("unknown library allowed to run")
	}
	doc.Workflows[0].Enabled = false
	if err := s.validateWorkflowLibrary(doc); err != nil {
		t.Fatal("unavailable library reference cannot be saved as draft")
	}
	s.Workflows = &workflow.Engine{}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/api/library/schedules", nil))
		if rec.Code != http.StatusNotImplemented {
			t.Fatal(method, rec.Code)
		}
	}
}

func TestThrottledWorkflowClaimRecoversOriginalEvent(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	w := workflow.Definition{ID: "issues", Name: "Implement", Repository: "acme/repo", Enabled: true, Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", Instructions: "Implement issue", IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash"}}
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	s.Workflows = &workflow.Engine{Store: store, Enabled: func() bool { return true }, Allowed: func(string) bool { return true }}
	run := issueRunConfig(GitHubRepo{FullName: w.Repository}, f.issue, config.GitHubIssueLinkReference)
	run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: "activation", Name: w.Name, Instructions: w.Task.Instructions}
	now := time.Now()
	s.GitHubCooldown.Now = func() time.Time { return now }
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		return execx.Result{Stderr: "gh: API rate limit exceeded"}, errors.New("exit status 1")
	})
	if _, err := s.SubmitWorkflowRun(t.Context(), run, f.submit); !errors.Is(err, workflow.ErrIssueAdmissionPending) {
		t.Fatal(err)
	}
	record := latestRecoveryRecord(t, s)
	if record.State != "retry_wait" || len(f.submissions) != 0 {
		t.Fatal(record.State, len(f.submissions))
	}
	expireIssueRetry(t, s, record.ID)
	now = now.Add(time.Minute)
	s.GitHubIssueRunner = f
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.submissions) != 1 || f.submissions[0].Workflow.EventID != "activation" {
		t.Fatal("lost activation or wrong submission count")
	}
}
