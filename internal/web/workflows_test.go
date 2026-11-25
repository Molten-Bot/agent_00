package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/library"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
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

func TestWorkflowRecoversPreviouslyClaimedIssue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		legacy, moveBranch bool
	}{
		{name: "pending event"},
		{name: "older journal with no run", legacy: true},
		{name: "branch moves during admission", moveBranch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := tc.legacy
			s, f := recoveryServer(t)
			ctx := t.Context()
			old := issueAttemptMetadata{InstanceID: "previous-runtime", Claim: config.GitHubIssueClaim{
				ID: "previous-claim", Attempt: 3, Branch: "agent00-issue-17", BaseBranch: "main", ExclusionLabel: "agent_00-in-progress",
			}}
			metadata, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			f.branch = true
			f.issue.Labels = append(f.issue.Labels, GitHubIssueLabel{Name: old.Claim.ExclusionLabel})
			f.refs["refs/tags/agent00-issue-17-claim-3"] = "old-tag"
			f.tags["old-tag"] = string(metadata)

			ahead := 0
			head := "old-head"
			moveOnLabel := false
			s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				args := strings.Join(cmd.Args, " ")
				switch {
				case strings.Contains(args, "--method POST") && strings.Contains(args, "/issues/17/labels") && moveOnLabel:
					head = "independent-commit"
					ahead = 1
				case strings.Contains(args, "/git/ref/heads/agent00-issue-17"):
					return execx.Result{Stdout: fmt.Sprintf(`{"object":{"sha":%q}}`, head)}, nil
				case strings.Contains(args, "/compare/base..."+head):
					result, err := json.Marshal(map[string]int{"ahead_by": ahead, "behind_by": 9})
					return execx.Result{Stdout: string(result)}, err
				}
				return f.Run(ctx, cmd)
			})
			doc, err := s.Workflows.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			w := &doc.Workflows[0]
			w.Trigger = workflow.Trigger{Event: "issue.labeled", Value: "ready"}
			w.IncludeBacklog = true
			w.ReviewCycles = 2
			w.Merge = workflow.Merge{Enabled: true, RequireApproval: true, Method: "rebase"}
			policy := w.ExecutionPolicyHash()
			if _, err := s.Workflows.Store.Save(doc); err != nil {
				t.Fatal(err)
			}
			engine := s.Workflows
			engine.StatePath = filepath.Join(t.TempDir(), "workflow-state.json")
			engine.Fetch = func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) {
				return []workflow.Item{{Number: 17, Kind: "issue", Repository: w.Repository, Open: true, Labels: []string{"ready"}}}, nil
			}
			var event string
			engine.Submit = func(ctx context.Context, r workflow.Run) (string, error) {
				event = r.ID
				run := issueRunConfig(GitHubRepo{FullName: w.Repository}, f.issue, config.GitHubIssueLinkClose)
				run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: r.ID, Name: w.Name, PolicyHash: policy, Instructions: w.Task.Instructions, ReviewCycles: w.ReviewCycles}
				id, err := s.SubmitWorkflowRun(ctx, run, f.submit)
				if legacy && errors.Is(err, workflow.ErrIssueAdmissionPending) {
					return "", errors.New("legacy admission did not retain the workflow run")
				}
				return id, err
			}
			err = engine.Tick(ctx)
			if (!legacy && !errors.Is(err, workflow.ErrIssueAdmissionPending)) || (legacy && err != nil) {
				t.Fatalf("scan error = %v", err)
			}
			observed := latestRecoveryRecord(t, s)
			if observed.State != "blocked" || observed.Incident != workflowIssueRecoveryIncident || !observed.NextAttemptAt.IsZero() ||
				observed.ID == old.Claim.ID || event == "" || len(f.submissions) != 0 {
				t.Fatalf("scan did not retain blocked observation: %+v", observed)
			}
			if legacy {
				// Emulate the persisted admission error and backoff from older versions.
				observed.State, observed.Incident, observed.Error = "retry_wait", "admission", "issue has an associated branch or closing PR; ownership requires inspection"
				observed.NextAttemptAt = time.Now().Add(-time.Minute)
				if err := s.IssueClaims.Save(observed); err != nil {
					t.Fatal(err)
				}
			}
			s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
			if err := s.ReconcileIssueWork(ctx); err != nil {
				t.Fatal(err)
			}
			runs, err := engine.Runs()
			if err != nil || (legacy && len(runs) != 0) || (!legacy && (len(runs) != 1 || runs[0].Status != "pending_issue")) || len(f.submissions) != 0 {
				t.Fatalf("unexpected reconciled runs: %+v, %v", runs, err)
			}

			server := httptest.NewServer(s.Handler())
			defer server.Close()
			request := func(method, path, body string, code int, want string) {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				var result map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(result)
				if err != nil || resp.StatusCode != code || !strings.Contains(string(encoded), want) {
					t.Fatalf("%s %s = %d %s, decode error %v", method, path, resp.StatusCode, encoded, err)
				}
			}
			const retryPath = "/api/github/automation/retry"
			const retryBody = `{"repository":"acme/repo","number":17,"expectedClaimId":"previous-claim","confirmStopped":true}`
			request(http.MethodGet, "/api/github/issues/17?repo=acme/repo", "", http.StatusOK, "resume through the workflow")
			request(http.MethodGet, retryPath+"?repo=acme/repo&number=17", "", http.StatusOK, old.Claim.ID)
			lock, err := s.IssueClaims.TryLock(localruntime.IssueExecutionKey(observed.Run.GitHubIssue))
			if err != nil || lock == nil {
				t.Fatalf("could not simulate an active worker: %v", err)
			}
			request(http.MethodPost, retryPath, retryBody, http.StatusConflict, "stop the active worker")
			lock.Close()
			if _, err := s.claimAndSubmitIssueMode(ctx, observed.Run, ReviewSettingsState{GitHubIssuesEnabled: true}, "", f.submit, true); !errors.Is(err, errIssueRecoveryRequired) {
				t.Fatalf("manual start bypassed workflow recovery: %v", err)
			}
			f.prs = recoveryPR("open", "null", old.Claim.Branch, "main")
			request(http.MethodPost, retryPath, retryBody, http.StatusConflict, "already has an open or merged PR")
			f.prs = "[]"
			ahead = 1
			request(http.MethodPost, retryPath, retryBody, http.StatusConflict, "inspect and reconcile commits")
			ahead = 0
			withDefinition := func(change func(*workflow.Definition)) {
				t.Helper()
				current, err := engine.Store.Load()
				if err != nil {
					t.Fatal(err)
				}
				change(&current.Workflows[0])
				if _, err = engine.Store.Save(current); err != nil {
					t.Fatal(err)
				}
			}
			withDefinition(func(w *workflow.Definition) { w.Enabled = false })
			request(http.MethodPost, retryPath, retryBody, http.StatusConflict, "originating workflow")
			withDefinition(func(w *workflow.Definition) { w.Enabled = true; w.ReviewCycles = 0 })
			request(http.MethodPost, retryPath, retryBody, http.StatusConflict, "original policy")
			withDefinition(func(w *workflow.Definition) { w.ReviewCycles = 2 })
			if len(f.refs) != 1 || len(f.submissions) != 0 {
				t.Fatal("blocked recovery modified remote ownership or submitted work")
			}

			f.issue.Body = "Updated issue requirements"
			if tc.moveBranch {
				moveOnLabel = true
				request(http.MethodPost, retryPath, retryBody, http.StatusConflict, "moved after verification")
				blocked := latestRecoveryRecord(t, s)
				if blocked.State != "blocked" || blocked.Incident != localruntime.IssueIncidentBranchRecovery ||
					blocked.ReservationSHA != "old-head" || len(f.submissions) != 0 {
					t.Fatalf("moved branch was not held for explicit recovery: %+v", blocked)
				}
				s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
				if err := s.ReconcileIssueWork(ctx); err != nil {
					t.Fatal(err)
				}
				request(http.MethodGet, "/api/github/issues/17?repo=acme/repo", "", http.StatusOK, "moved after verification")
				body := strings.Replace(retryBody, old.Claim.ID, blocked.ID, 1)
				request(http.MethodPost, retryPath, body, http.StatusConflict, "inspect and reconcile commits")
				if len(f.submissions) != 0 {
					t.Fatal("retry admitted independent commits")
				}
				head, ahead, moveOnLabel = "old-head", 0, false
				request(http.MethodPost, retryPath, body, http.StatusAccepted, "request-1")
			} else {
				request(http.MethodPost, retryPath, retryBody, http.StatusAccepted, "request-1")
			}
			request(http.MethodPost, retryPath, retryBody, http.StatusConflict, "claim changed")
			if len(f.submissions) != 1 {
				t.Fatalf("submitted %d attempts", len(f.submissions))
			}
			run := f.submissions[0]
			claim := run.GitHubIssue.Claim
			attempt := 4
			if tc.moveBranch {
				attempt++
			}
			if claim.Attempt != attempt || claim.Branch != old.Claim.Branch || claim.BaseBranch != "main" || run.GitHubIssue.LinkMode != config.GitHubIssueLinkClose ||
				run.Workflow == nil || run.Workflow.EventID != event || run.Workflow.PolicyHash != policy || run.Workflow.ReviewCycles != 2 ||
				!strings.Contains(run.Prompt, "Updated issue requirements") {
				t.Fatalf("recovery lost branch or workflow settings: %+v", run)
			}
			record := latestRecoveryRecord(t, s)
			if record.ReservationSHA != "old-head" || record.RequestID != "request-1" {
				t.Fatalf("recovery did not preserve the reserved head: %+v", record)
			}
			for _, cmd := range f.trace {
				if strings.Contains(cmd, "createLinkedBranch") || strings.Contains(cmd, "--method PATCH") || strings.Contains(cmd, "--method DELETE") {
					t.Fatalf("recovery changed the existing branch: %s", cmd)
				}
			}

			engine.Reconcile = func(id string) string {
				if id == run.Workflow.EventID {
					return record.RequestID
				}
				return ""
			}
			const prURL = "https://github.com/acme/repo/pull/42"
			if err := engine.Complete(event, workflow.Outcome{Status: "succeeded", PRURL: prURL}); err != nil {
				t.Fatal(err)
			}
			approved, merges := false, 0
			engine.InspectPR = func(context.Context, string, string) (workflow.Item, error) {
				return workflow.Item{Open: true, ChecksPassed: true, HumanApproved: approved, HeadSHA: "reviewed-head"}, nil
			}
			engine.Merge = func(_ context.Context, r workflow.Run) error {
				merges++
				if r.ID != event || r.Workflow.Merge.Method != "rebase" || r.MergeHead != "reviewed-head" {
					t.Errorf("merge lost original policy or reviewed head: %+v", r)
				}
				return nil
			}
			if err := engine.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, err = engine.Runs()
			if err != nil || len(runs) != 1 || !strings.Contains(runs[0].Reason, "submitted clean review") || merges != 0 {
				t.Fatalf("missing native review did not block recovered work: %+v, merges=%d, %v", runs, merges, err)
			}
			receipt := recoveredWorkflowReview(*w, prURL)
			if err := engine.Complete(event, workflow.Outcome{Status: "succeeded", PRURL: prURL, ReviewReceipt: receipt}); err != nil {
				t.Fatal(err)
			}
			if err := engine.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, err = engine.Runs()
			if err != nil || len(runs) != 1 || runs[0].RequestID != "request-1" || runs[0].Reason != "Waiting for human approval" || merges != 0 {
				t.Fatalf("journal did not link recovered work or enforce approval: %+v, merges=%d, %v", runs, merges, err)
			}
			approved = true
			if err := engine.Tick(ctx); err != nil || merges != 1 {
				t.Fatalf("approved merge count=%d, error=%v", merges, err)
			}
		})
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
		if rec.Code != http.StatusGone {
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

func TestLoopbackScheduleStatusDuringFetch(t *testing.T) {
	s, w := inputWorkflowServer(t, "prompt")
	schedule := w
	schedule.ID = "timer"
	schedule.Trigger = workflow.Trigger{Event: "schedule", IntervalHours: 2}
	w.Trigger = workflow.Trigger{Event: "issue.opened"}
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows = []workflow.Definition{schedule, w}
	if _, err = s.Workflows.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.Workflows.Now = func() time.Time { return now }
	started, resume := make(chan struct{}), make(chan struct{})
	s.Workflows.Fetch = func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) {
		close(started)
		<-resume
		return nil, nil
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- s.Workflows.Tick(t.Context()) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("tick ended before fetch: %v", err)
	case <-time.After(3 * time.Second):
		close(resume)
		t.Fatal("fetch did not start")
	}
	defer func() {
		close(resume)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := server.Client()
	client.Timeout = time.Second
	start := time.Now()
	resp, err := client.Get(server.URL + "/api/workflows/schedules")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status []workflow.ScheduleStatus
	if err = json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if resp.StatusCode != 200 || len(status) != 1 || !status[0].NextRunAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("HTTP %d schedules=%+v", resp.StatusCode, status)
	}
	t.Logf("loopback schedule status during blocked fetch: %v", elapsed)
}

func recoveredWorkflowReview(w workflow.Definition, prURL string) *githubutil.ReviewReceipt {
	return &githubutil.ReviewReceipt{PRURL: prURL, Head: "reviewed-head", Run: "run", Pass: 1, ReviewID: 42,
		Marker: "<!-- test review -->", URL: prURL + "#pullrequestreview-42",
		Policy: githubutil.ReviewPolicy{FixPasses: w.ReviewCycles, Hash: w.ExecutionPolicyHash()},
		Result: json.RawMessage(`{"status":"clean","mergeReady":true,"findings":[]}`)}
}
