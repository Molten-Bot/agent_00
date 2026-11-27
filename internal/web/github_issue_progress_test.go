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
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestIssuePlanningFollowsVerifiedDelivery(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	state := "open"
	status := issueTodo
	var updates []string
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		args := strings.Replace(strings.Join(cmd.Args, " "), "api --include ", "api ", 1)
		switch {
		case strings.HasPrefix(args, "api --method PATCH repos/acme/repo/issues/17"):
			status = strings.TrimPrefix(cmd.Args[len(cmd.Args)-1], "status=")
			updates = append(updates, status)
			fallthrough
		case strings.HasPrefix(args, "api repos/acme/repo/issues/17 -H"):
			return execx.Result{Stdout: fmt.Sprintf(`{"state":%q,"status":{"name":%q}}`, state, status)}, nil
		default:
			res, err := f.Run(ctx, cmd)
			if strings.Contains(args, "defaultBranchRef") {
				res.Stdout = strings.Replace(res.Stdout, `"repository":{`, `"repository":{"planningStatuses":{"totalCount":7},`, 1)
				res.Stdout = strings.Replace(res.Stdout, `"issue":{`, fmt.Sprintf(`"issue":{"status":{"name":%q},`, status), 1)
			}
			return res, err
		}
	})

	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Run.GitHubIssue.LinkMode = config.GitHubIssueLinkClose
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.RunError = "review: pull request head changed"
	f.prs = recoveryPR("open", "null", record.Run.GitHubIssue.Claim.Branch, "main")
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	record = latestRecoveryRecord(t, s)
	if record.State != "fulfilled" || record.PlanningStatus != issueInReview || record.Outcome != "error" {
		t.Fatalf("open draft and failed worker = %s %s %s", record.State, record.PlanningStatus, record.Outcome)
	}

	record.Outcome = "completed"
	record.ReviewRequired = true
	record.ReviewHead = "superseded-head"
	f.prs = recoveryPR("closed", `"2026-05-12T00:00:00Z"`, record.Run.GitHubIssue.Claim.Branch, "main")
	if err := s.syncIssuePlanning(t.Context(), record); err != nil || status != issueInReview {
		t.Fatalf("stale review advanced issue: %s %v", status, err)
	}
	record.ReviewHead = "existing-commits"
	f.prs = recoveryPR("closed", "null", record.Run.GitHubIssue.Claim.Branch, "main")
	if err := s.syncIssuePlanning(t.Context(), record); err != nil || status != issueInReview {
		t.Fatalf("closed unmerged PR advanced issue: %s %v", status, err)
	}
	f.prs = recoveryPR("closed", `"2026-05-12T00:00:00Z"`, record.Run.GitHubIssue.Claim.Branch, "main")
	record.Run.GitHubIssue.LinkMode = config.GitHubIssueLinkReference
	if err := s.syncIssuePlanning(t.Context(), record); err != nil || status != issueInReview {
		t.Fatalf("reference-only PR closed issue: %s %v", status, err)
	}
	record.Run.GitHubIssue.LinkMode = config.GitHubIssueLinkClose
	state = "closed"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(updates, ","); got != "In progress,In review,Done" || latestRecoveryRecord(t, s).PlanningStatus != issueDone {
		t.Fatalf("issue status transitions = %s", got)
	}
}

func TestIssuePlanningBacklogRequiresExplicitStart(t *testing.T) {
	t.Parallel()
	var writes int
	s := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		writes++
		if !strings.Contains(strings.Join(cmd.Args, " "), "status=Todo") {
			t.Fatalf("expected Todo write: %v", cmd.Args)
		}
		return execx.Result{Stdout: `{"state":"open","status":{"name":"Todo"}}`}, nil
	}), true)
	issue := &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17}
	target := issueClaimTarget{PlanningEnabled: true, PlanningStatus: "Backlog"}
	if _, err := s.prepareIssuePlanning(t.Context(), issue, target, false); err == nil || writes != 0 {
		t.Fatalf("automatic admission accepted Backlog: writes=%d error=%v", writes, err)
	}
	if got, err := s.prepareIssuePlanning(t.Context(), issue, target, true); got != issueTodo || err != nil || writes != 1 {
		t.Fatalf("manual admission failed to plan Backlog: status=%q writes=%d error=%v", got, writes, err)
	}
}

func TestIssueTaskSnapshotKeepsRetryAndCompletionOnOneTask(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, workflow.OutcomeReviewPending, 1)
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.PlanningStatus = issueInReview
	record.State, record.Step = "retry_wait", "review"
	record.Error = "review: pull request head changed"
	record.RunError = record.Error
	record.NextAttemptAt = time.Now().Add(time.Minute)
	save := func(r localruntime.IssueClaimRecord, status string) {
		t.Helper()
		if err := s.IssueClaims.Save(r); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(r.Run)
		if err != nil {
			t.Fatal(err)
		}
		s.Broker.RecordTaskRunConfig(r.RequestID, body)
		s.Broker.IngestLog(fmt.Sprintf("task status=%s request_id=%s", status, r.RequestID))
	}
	save(record, "error")
	snapshot := func() (Task, int) {
		t.Helper()
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var state Snapshot
		if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil || len(state.Tasks) != 1 {
			t.Fatalf("expected one task, status=%d rows=%d err=%v", res.Code, len(state.Tasks), err)
		}
		return state.Tasks[0], state.Stats.TotalTasks
	}
	waiting, _ := snapshot()
	if waiting.Status != "queued" || waiting.RetryAt == "" || waiting.Error != record.Error || waiting.TaskID == "" {
		t.Fatalf("retry wait missing: %+v", waiting)
	}

	retry := record
	retry.ID, retry.RequestID = newIssueClaimID(), "request-2"
	issue, policy := *record.Run.GitHubIssue, *record.Run.Workflow
	claim := *issue.Claim
	claim.ID, claim.Attempt = retry.ID, 2
	issue.Claim, policy.EventID = &claim, "retry-event"
	retry.Run.GitHubIssue, retry.Run.Workflow = &issue, &policy
	retry.State, retry.Step, retry.Outcome, retry.Error = "running", "checks", "", ""
	retry.RunError, retry.PRURL, retry.NextAttemptAt = "", "", time.Time{}
	save(retry, "running")
	current, total := snapshot()
	if current.TaskID != waiting.TaskID || current.RequestID != retry.RequestID || total != 1 ||
		current.PRURL != record.PRURL || current.GitHubIssueURL != issue.URL || current.WorkflowID != policy.ID ||
		len(current.IssueAttempts) != 2 || current.IssueAttempts[0].Error != record.Error {
		t.Fatalf("retry changed task identity or lost evidence: %+v total=%d", current, total)
	}
	retry.State, retry.Outcome, retry.RunError = "blocked", "error", "pr_verification: persistent failure"
	save(retry, "error")
	blocked, _ := snapshot()
	if blocked.TaskID != waiting.TaskID || blocked.Status != "error" || blocked.Error != retry.RunError || !blocked.CanRerun {
		t.Fatalf("persistent failure missing: %+v", blocked)
	}
	retry.Incident = localruntime.IssueIncidentBranchRecovery
	retry.Error = "Reopen the existing pull request before using Recover / retry"
	save(retry, "error")
	held, _ := snapshot()
	if held.CanRerun || held.Error != retry.Error || held.TaskID != blocked.TaskID {
		t.Fatalf("PR recovery prerequisite still exposes task retry: %+v", held)
	}
	retry.Incident, retry.Error = "", ""
	save(retry, "error")
	s.Broker = NewBroker()
	restoredBlocked, _ := snapshot()
	if restoredBlocked.TaskID != blocked.TaskID || restoredBlocked.Status != "error" || !restoredBlocked.HistoryOnly {
		t.Fatalf("restart lost blocked task: %+v", restoredBlocked)
	}

	retry.State, retry.Outcome, retry.RunError, retry.PlanningStatus = "fulfilled", "completed", "", issueDone
	save(retry, "completed")
	done, total := snapshot()
	if done.TaskID != waiting.TaskID || done.Status != "merged" || len(done.IssueAttempts) != 2 || total != 1 {
		t.Fatalf("completion did not update original task: %+v total=%d", done, total)
	}
	s.Broker = NewBroker()
	restored, _ := snapshot()
	if restored.TaskID != done.TaskID || restored.Status != "merged" || len(restored.IssueAttempts) != 2 ||
		restored.PRURL != record.PRURL {
		t.Fatalf("restart lost issue task history: %+v", restored)
	}
	if len(f.submissions) != 1 {
		t.Fatalf("test fixture submitted %d issue tasks", len(f.submissions))
	}
}

func TestIssueTaskUsesDurableStateAfterRestart(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, workflow.OutcomeReviewPending, 1)
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.RunError = "review: the current head has no clean review"
	definitions, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	run := workflow.Run{
		ID: record.Run.Workflow.EventID, RequestID: record.RequestID, PRURL: record.PRURL,
		Workflow: definitions.Workflows[0], Status: "awaiting_review",
		Reason: "Waiting for unresolved review threads", GitHubRetryAt: time.Now().Add(time.Minute),
		Outcome: &workflow.Outcome{Status: workflow.OutcomeReviewPending, Reason: record.RunError},
	}
	journal, err := json.Marshal(map[string]any{
		"version": 2, "observations": map[string]any{}, "baselines": map[string]bool{}, "runs": []workflow.Run{run},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Workflows.StatePath, journal, 0600); err != nil {
		t.Fatal(err)
	}
	s.Broker = NewBroker()
	s.ResolveTaskControls = func(string) TaskControls { return TaskControls{Run: true} }
	taskID := issueTaskID(record.Run.GitHubIssue, record.Run.Workflow)
	for _, tc := range []struct{ state, status, issueState string }{
		{"verifying", "queued", "awaiting_review"},
		{localruntime.IssueChecksPending, "queued", localruntime.IssueChecksPending},
		{"blocked", "error", "blocked"},
		{localruntime.IssueStateMergedUnverified, "merged", localruntime.IssueStateMerged},
	} {
		t.Run(tc.state, func(t *testing.T) {
			record.State, record.Error = tc.state, mergedUnverifiedIssueError
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			tasks := s.snapshot().Tasks
			if len(tasks) != 1 || tasks[0].TaskID != taskID || tasks[0].Status != tc.status || tasks[0].IssueState != tc.issueState {
				t.Fatalf("task status after restart: %+v", tasks)
			}
			task := tasks[0]
			if tc.status == "merged" && (task.RetryAt != "" || task.ReviewGate != "" || task.Error != "" ||
				task.CanRerun || task.Controls.Run || task.IssueAttempts[0].Error != record.RunError ||
				task.IssueAttempts[0].Detail != mergedUnverifiedIssueError) {
				t.Fatalf("merged task lost history or kept active controls: %+v", task)
			}
		})
	}

	var transitions []string
	s.Workflows.RecordIssueMerge = s.RecordIssueMerge
	s.Workflows.Logf = func(format string, args ...any) {
		transitions = append(transitions, fmt.Sprintf(format, args...))
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Workflows.Runs()
	if err != nil || len(runs) != 0 || len(f.submissions) != 1 || len(transitions) != 1 ||
		!strings.Contains(transitions[0], "status=merged reason=external_merge") {
		t.Fatalf("merged claim did not settle stale workflow: %+v, transitions %v (err %v)", runs, transitions, err)
	}
	task := s.snapshot().Tasks[0]
	if task.ReviewGate != "" || task.IssueAttempts[0].ReviewDetail != run.Reason {
		t.Fatalf("pruned workflow gate was not kept in attempt history: %+v", task)
	}
}

func TestWorkflowMergeObservationSurvivesRestartWithoutPRLookup(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"app merge", "external merge"} {
		t.Run(name, func(t *testing.T) {
			testWorkflowMergeSurvivesRestartWithoutPRLookup(t, name == "app merge")
		})
	}
}

func testWorkflowMergeSurvivesRestartWithoutPRLookup(t *testing.T, mergedByApp bool) {
	t.Helper()
	s, f := recoveryServer(t)
	engine := s.Workflows
	doc, err := engine.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows[0].Merge.Enabled = true
	doc.Workflows[0].Merge.RequireApproval = false
	if doc, err = engine.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "completed", 1)
	record.PRURL = "https://github.com/acme/repo/pull/42"
	wantState, wantReviewDetail := localruntime.IssueStateMerged, ""
	mergedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !mergedByApp {
		record.Outcome = "error"
		record.PRVerification = "Verify draft on the current head."
		record.RunError = "pr_verification: checks never finished"
		wantState, wantReviewDetail = localruntime.IssueStateMergedUnverified, "Waiting for successful checks"
	}
	record.NextAttemptAt = time.Now().Add(time.Hour)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	run := workflow.Run{
		ID: record.Run.Workflow.EventID, RequestID: record.RequestID, PRURL: record.PRURL,
		Workflow: doc.Workflows[0], Status: "waiting", Reason: "Waiting for successful checks",
		Item: workflow.Item{Repository: "acme/repo", Kind: "issue", Number: 17},
	}
	journal, err := json.Marshal(map[string]any{
		"version": 2, "observations": map[string]any{}, "baselines": map[string]bool{}, "runs": []workflow.Run{run},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.StatePath, journal, 0600); err != nil {
		t.Fatal(err)
	}
	merges := 0
	engine.InspectPR = func(context.Context, string, string) (workflow.Item, error) {
		if !mergedByApp || merges > 0 {
			return workflow.Item{Merged: true, MergedAt: mergedAt, Title: recoveryPRTitle}, nil
		}
		return workflow.Item{Open: true, Title: recoveryPRTitle, HeadSHA: "reviewed-head", ChecksPassed: true}, nil
	}
	engine.Merge = func(context.Context, workflow.Run) error {
		merges++
		return nil
	}
	engine.Fetch = func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) {
		return nil, nil
	}
	claimWriteErr := errors.New("claim receipt write failed")
	engine.RecordIssueMerge = func(workflow.Run, workflow.Item, bool) error {
		return claimWriteErr
	}
	if err := engine.Tick(t.Context()); !errors.Is(err, claimWriteErr) {
		t.Fatalf("merge checkpoint did not survive failed claim write: %v", err)
	}
	restarted := s
	restarted.Broker = NewBroker()
	restarted.Workflows = &workflow.Engine{StatePath: engine.StatePath}
	checkpoint := restarted.snapshot().Tasks
	if len(checkpoint) != 1 || checkpoint[0].Status != "merged" || checkpoint[0].CanRerun ||
		checkpoint[0].PRURL != record.PRURL || checkpoint[0].RetryAt != "" || checkpoint[0].ReviewGate != "" ||
		checkpoint[0].IssueAttempts[0].ReviewDetail != run.Reason {
		t.Fatalf("task did not honor the confirmed merge checkpoint: %+v", checkpoint)
	}
	if mergedByApp {
		pendingRun := run
		if err := json.Unmarshal([]byte(`{"appMerge":{"pendingVerification":true}}`), &pendingRun); err != nil {
			t.Fatal(err)
		}
		pendingJournal, err := json.Marshal(map[string]any{"version": 2, "runs": []workflow.Run{pendingRun}})
		if err != nil {
			t.Fatal(err)
		}
		pending := restarted
		pending.Workflows = &workflow.Engine{StatePath: filepath.Join(t.TempDir(), "state.json")}
		if err := os.WriteFile(pending.Workflows.StatePath, pendingJournal, 0600); err != nil {
			t.Fatal(err)
		}
		tasks := pending.snapshot().Tasks
		if len(tasks) != 1 || tasks[0].Status != "queued" || tasks[0].IssueState != "waiting" {
			t.Fatalf("unconfirmed app merge settled the task: %+v", tasks)
		}
	}
	// A checkpoint about another PR must not override the claim's linked PR.
	if err := s.IssueClaims.Update(record.ID, func(r *localruntime.IssueClaimRecord) error {
		r.PRURL = "https://github.com/acme/repo/pull/43"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if task := restarted.snapshot().Tasks[0]; task.Status == "merged" || !strings.HasSuffix(task.PRURL, "/43") {
		t.Fatalf("unrelated merge checkpoint settled the task: %+v", task)
	}
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	engine.RecordIssueMerge = s.RecordIssueMerge
	// List must still return the readable claim when a neighboring receipt
	// cannot be decoded.
	if err := os.WriteFile(filepath.Join(s.IssueClaims.Root, "malformed.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := engine.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.IssueClaims.Root, "malformed.json")); err != nil {
		t.Fatal(err)
	}
	if (merges == 1) != mergedByApp || merges > 1 {
		t.Fatalf("app merge count = %d, expected app merge = %t", merges, mergedByApp)
	}
	if runs, err := engine.Runs(); err != nil || len(runs) != 0 {
		t.Fatalf("workflow still active after observing merge: %+v (%v)", runs, err)
	}
	// A concurrent recovery pass may still hold a pre-merge snapshot.
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	// Cleanup may also hold a terminal snapshot from before the full receipt.
	stale, err := s.IssueClaims.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale.PRMergedAt, stale.ReviewDetail = time.Time{}, ""
	stale.PRMergedByApp, stale.UnlockPending = false, false
	if err := s.IssueClaims.Save(stale); err != nil {
		t.Fatal(err)
	}
	// Simulate restart before the issue reconciler can look up the linked PR.
	s.Broker = NewBroker()
	s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	s.Workflows = &workflow.Engine{StatePath: engine.StatePath}
	f.prs = "invalid pull request response"
	merged := latestRecoveryRecord(t, s)
	if merged.State != wantState || !merged.PRMergedAt.Equal(mergedAt) || merged.PRMergedByApp != mergedByApp ||
		merged.PRTitle != recoveryPRTitle || !merged.NextAttemptAt.IsZero() || merged.ReviewDetail != wantReviewDetail {
		t.Fatalf("workflow pruned before persisting the merge receipt: %+v", merged)
	}
	tasks := s.snapshot().Tasks
	if len(tasks) != 1 || tasks[0].Status != "merged" || tasks[0].CanRerun ||
		tasks[0].IssueAttempts[0].Error != record.RunError {
		t.Fatalf("restart lost the merged task or its verification evidence: %+v", tasks)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatalf("merged claim still depends on PR lookup: %v", err)
	}
}

func TestPRMonitorPersistsIssueMergeBeforeRetiringBrokerTask(t *testing.T) {
	t.Parallel()
	s, _ := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "completed", 1)
	// A continuation can reuse the broker request ID on a new claim.
	previous := record
	previous.ID = strings.Repeat("0", 32)
	issue := *record.Run.GitHubIssue
	claim := *issue.Claim
	claim.ID = previous.ID
	issue.Claim = &claim
	previous.Run.GitHubIssue = &issue
	if err := s.IssueClaims.Save(previous); err != nil {
		t.Fatal(err)
	}
	record.Run.GitHubIssue.Claim.Attempt++
	record.PRVerification = "Draft checks pending"
	record.RunError = "checks never finished"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	const prURL = "https://github.com/acme/repo/pull/42"
	s.Broker.IngestLog("task status=completed request_id=" + record.RequestID + " pr_url=" + prURL)
	task, ok := s.Broker.Task(record.RequestID)
	if !ok {
		t.Fatal("monitor task missing")
	}
	monitor := PRMergeMonitor{Broker: s.Broker, Logf: t.Logf, RecordIssueMerge: s.RecordMonitoredIssueMerge}
	const mergedAt = "2026-02-03T04:05:06Z"
	monitor.applyPRState(t.Context(), task, prViewState{State: "MERGED", Title: recoveryPRTitle, MergedAt: mergedAt}, nil)
	task, _ = s.Broker.Task(record.RequestID)
	if task.Status != "merged" {
		t.Fatalf("broker status = %q, want merged", task.Status)
	}

	s.Broker = NewBroker()
	s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	if first, err := s.IssueClaims.Load(previous.ID); err != nil || first.PRMerged() {
		t.Fatalf("monitor changed the predecessor claim: %+v, %v", first, err)
	}
	got := latestRecoveryRecord(t, s)
	if got.State != localruntime.IssueStateMergedUnverified || got.PRURL != prURL || got.PRTitle != recoveryPRTitle ||
		formatTimeOrEmpty(got.PRMergedAt) != mergedAt || got.PRMergedByApp || got.RunError != record.RunError {
		t.Fatalf("restart lost monitored merge evidence: %+v", got)
	}
	tasks := s.snapshot().Tasks
	if len(tasks) != 1 || tasks[0].Status != "merged" || tasks[0].CanRerun || len(tasks[0].IssueAttempts) != 2 {
		t.Fatalf("restart lost terminal task status: %+v", tasks)
	}
}

func TestIssueRecoveryUsesWorkflowLinkedPRBeforeBranchDiscovery(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "completed", 1)
	record.PRURL = ""
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	const linkedURL = "https://github.com/acme/repo/pull/42"
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	run := workflow.Run{
		ID: record.Run.Workflow.EventID, RequestID: record.RequestID, Workflow: doc.Workflows[0], Status: "running",
		Item:    workflow.Item{Repository: "acme/repo", Kind: "issue", Number: 17},
		Outcome: &workflow.Outcome{Status: workflow.OutcomeReviewPending, PRURL: linkedURL},
	}
	journal, err := json.Marshal(map[string]any{
		"version": 2, "observations": map[string]any{}, "baselines": map[string]bool{}, "runs": []workflow.Run{run},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Workflows.StatePath, journal, 0600); err != nil {
		t.Fatal(err)
	}
	s.Broker = NewBroker()
	f.prs = fmt.Sprintf(`[
		{"number":43,"state":"closed","merged_at":"2026-02-03T04:05:07Z",
		 "head":{"ref":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main","repo":{"full_name":"acme/repo"}}},
		{"number":42,"state":"closed","merged_at":"2026-02-03T04:05:06Z",
		 "head":{"ref":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main","repo":{"full_name":"acme/repo"}}}
	]`, record.Run.GitHubIssue.Claim.Branch, record.Run.GitHubIssue.Claim.Branch)
	if err := s.reconcileIssueRecord(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	if !got.PRMerged() || got.PRURL != linkedURL {
		t.Fatalf("recovery did not settle the workflow-linked pull request: %+v", got)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || response.Code != http.StatusOK ||
		len(status.Tasks) != 1 || status.Tasks[0].Status != "merged" || status.Tasks[0].PRURL != linkedURL ||
		status.Tasks[0].CanRerun {
		t.Fatalf("issue task status endpoint did not report terminal success: HTTP %d, tasks %+v, err %v", response.Code, status.Tasks, err)
	}
}

func TestOrdinaryPRMonitorContinuesWithUnreadableIssueClaim(t *testing.T) {
	t.Parallel()
	s, _ := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := latestRecoveryRecord(t, s)
	if err := os.WriteFile(filepath.Join(s.IssueClaims.Root, record.ID+".json"), []byte("invalid claim"), 0600); err != nil {
		t.Fatal(err)
	}
	monitor := PRMergeMonitor{Broker: s.Broker, Logf: t.Logf, RecordIssueMerge: s.RecordMonitoredIssueMerge}
	const prURL = "https://github.com/acme/repo/pull/42"
	for _, source := range []string{"prompt", "library", "github_issues"} {
		s.Broker.IngestLog("task status=completed request_id=" + source + " pr_url=" + prURL)
		s.Broker.RecordTaskSource(source, source)
		task, _ := s.Broker.Task(source)
		monitor.applyPRState(t.Context(), task, prViewState{State: "MERGED", MergedAt: "2026-02-03T04:05:06Z"}, nil)
		task, _ = s.Broker.Task(source)
		if (task.Status == "merged") != (source != "github_issues") {
			t.Errorf("task source %s ended with status %q", source, task.Status)
		}
	}
}

func TestIssueRecoveryBackfillsReleaseMergeTimeAfterTaskRetires(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	now := time.Now()
	s.Broker.now = func() time.Time { return now }
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "completed", 1)
	record.State, record.PRURL = localruntime.IssueStateMerged, "https://github.com/acme/repo/pull/42"
	record.PRMergedByApp = true
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	s.Broker.IngestLog("task status=completed request_id=" + record.RequestID + " pr_url=" + record.PRURL)
	if err := s.reconcileIssueRecord(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	task, ok := s.Broker.Task(record.RequestID)
	releases := s.Broker.Snapshot().Releases
	const mergedAt = "2026-02-03T04:05:06Z"
	if !ok || task.Status != "merged" || len(releases) != 1 || releases[0].MergedAt == mergedAt {
		t.Fatalf("task and provisional release after merge: %+v, %+v", task, releases)
	}
	provisional := releases[0]
	if err := s.Broker.CloseTask(record.RequestID); err != nil {
		t.Fatal(err)
	}
	f.prs = `[{"number":42,"state":"closed","merged_at":"` + mergedAt + `"}]`
	now = now.Add(retainedIssuePRPollInterval)
	if err := s.reconcileIssueRecord(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	releases = s.Broker.Snapshot().Releases
	if !got.PRMerged() || !got.PRMergedByApp || formatTimeOrEmpty(got.PRMergedAt) != mergedAt ||
		len(releases) != 1 || releases[0].MergedAt != mergedAt || releases[0].ReleasedAt != provisional.ReleasedAt {
		t.Fatalf("missing GitHub merge time or release metadata: %+v, %+v", got, releases)
	}
}
