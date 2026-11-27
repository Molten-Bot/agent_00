package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestRetainMergedIssueLogs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		record localruntime.IssueClaimRecord
		want   bool
	}{
		{name: "clean merge", record: localruntime.IssueClaimRecord{Outcome: "merged"}},
		{name: "unfinished verification", record: localruntime.IssueClaimRecord{Outcome: "merged", PRVerification: "Verify draft checks"}, want: true},
		{name: "failed review", record: localruntime.IssueClaimRecord{Outcome: "merged", RunError: "review: interrupted"}, want: true},
		{name: "no issue claim"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retainMergedIssueLogs(test.record); got != test.want {
				t.Fatalf("retainMergedIssueLogs() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRetainMergedReviewLogs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		outcome *workflow.Outcome
		want    bool
	}{
		{name: "no archived review"},
		{name: "clean review", outcome: &workflow.Outcome{Status: "succeeded"}},
		{name: "failed review", outcome: &workflow.Outcome{Status: "failed"}, want: true},
		{name: "pending review", outcome: &workflow.Outcome{Status: workflow.OutcomeReviewPending}, want: true},
		{name: "interrupted review", outcome: &workflow.Outcome{Status: "succeeded", ReviewInterrupted: true}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			const requestID = "pr-workflow"
			if test.outcome != nil {
				if err := workflow.SaveReviewOutcome(root, requestID, *test.outcome); err != nil {
					t.Fatal(err)
				}
			}
			if retain, err := retainMergedReviewLogs(root, requestID); err != nil || retain != test.want {
				t.Fatalf("retainMergedReviewLogs() = %t, %v, want %t", retain, err, test.want)
			}
		})
	}
}

func TestIssueOwnershipCheckPublishesOperatorStop(t *testing.T) {
	t.Parallel()

	store, record := lifecycleRecord(t)
	work, _, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
	if err != nil || work == nil {
		t.Fatalf("prepare issue execution = %v, %v", work, err)
	}
	defer work.lock.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	controller := newLocalTaskController()
	handle := controller.Register(record.RequestID, cancel)
	broker := web.NewBroker()
	broker.RecordTaskRunConfig(record.RequestID, []byte(`{"repo":"https://github.com/acme/repo","prompt":"fix issue"}`))

	outcome := checkLocalIssueOwnership(ctx, record.Run, record.RequestID, handle, func(ctx context.Context, _ config.Config) error {
		if err := controller.Stop(record.RequestID); err != nil {
			t.Fatal(err)
		}
		return fmt.Errorf("verify issue ownership: %w", ctx.Err())
	}, func(format string, args ...any) {
		broker.IngestLog(fmt.Sprintf(format, args...))
	})
	controller.Complete(record.RequestID)
	if outcome.State != "stopped" || !errors.Is(outcome.Result.Err, context.Canceled) {
		t.Fatalf("ownership outcome = %+v", outcome)
	}
	task, ok := broker.Task(record.RequestID)
	if !ok || task.Status != "stopped" {
		t.Fatalf("dashboard task found = %v, status = %q", ok, task.Status)
	}
	if err := work.finish(ctx, outcome.State, outcome.Result, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load(record.ID)
	if err != nil || stored.Outcome != "stopped" {
		t.Fatalf("stored outcome = %q, %v", stored.Outcome, err)
	}
}

func lifecycleRecord(t *testing.T) (*localruntime.IssueClaimStore, localruntime.IssueClaimRecord) {
	t.Helper()
	store := &localruntime.IssueClaimStore{Root: filepath.Join(t.TempDir(), "claims")}
	owner, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	record := localruntime.IssueClaimRecord{Version: 2, ID: id, InstanceID: owner, RequestID: "issue-" + id, LabelAdded: true, Run: config.Config{RepoURL: "https://github.com/acme/repo", GitHubIssue: &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, Claim: &config.GitHubIssueClaim{ID: id, Branch: "agent00-issue-17", BaseBranch: "main", Attempt: 1}}}}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	return store, record
}

func TestIssueExecutionIdempotenceAndTerminalReceipt(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	work, duplicate, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
	if err != nil || work == nil || duplicate != "" {
		t.Fatalf("prepare=%v %s %v", work, duplicate, err)
	}
	if work2, duplicate, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{}); err != nil || work2 != nil || duplicate != record.RequestID {
		t.Fatalf("duplicate=%v %s %v", work2, duplicate, err)
	}
	if err := work.finish(context.Background(), "error", app.Result{ExitCode: app.ExitGit, Err: errors.New("push failed")}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UnlockPending || got.StoppedAt.IsZero() || got.Outcome != "error" || got.FailureDetail() != "push failed" || got.ExitCode != app.ExitGit || got.WorkspaceDir == "" {
		t.Fatalf("lost terminal evidence: %+v", got)
	}
	if work2, duplicate, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{}); err != nil || work2 != nil || duplicate != record.RequestID {
		t.Fatal("completed attempt dispatched again")
	}
}

func TestIssueExecutionClearsVerificationOnlyOnVerifiedCompletion(t *testing.T) {
	t.Parallel()
	const requirement = "Verify the submitted review on the draft head."
	for _, tc := range []struct {
		state  string
		passed bool
		want   string
	}{
		{state: "completed", want: requirement},
		{state: "stopped", passed: true, want: requirement},
		{state: "completed", passed: true},
	} {
		t.Run(fmt.Sprintf("%s/verification_passed=%t", tc.state, tc.passed), func(t *testing.T) {
			store, record := lifecycleRecord(t)
			record.PRVerification = requirement
			if err := store.Save(record); err != nil {
				t.Fatal(err)
			}
			work, _, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
			if err != nil || work == nil {
				t.Fatalf("prepare issue execution = %v, %v", work, err)
			}
			result := app.Result{PRURL: "https://github.com/acme/repo/pull/42", PRVerificationPassed: tc.passed}
			if err := work.finish(t.Context(), tc.state, result, nil); err != nil {
				t.Fatal(err)
			}
			got, err := store.Load(record.ID)
			if err != nil || got.Outcome != tc.state || got.PRVerification != tc.want {
				t.Fatalf("saved outcome=%q verification=%q error=%v, want %q and %q", got.Outcome, got.PRVerification, err, tc.state, tc.want)
			}
		})
	}
}

func TestIssueCheckWaitResumesSameClaimAndPreservesBackoff(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	record.WorkspaceDir = filepath.Join(filepath.Dir(store.Root), "issue-workspaces", record.ID)
	record.CheckoutComplete, record.PublicationReady, record.AgentAttempts = true, true, 3
	record.PRVerification = "Verify CI and clean review on this draft."
	record.Run.Workflow = &config.WorkflowRunConfig{ID: "issues", EventID: "activation", PolicyHash: "policy"}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	policy := localruntime.IssueRecoveryConfig{RetryDelaySeconds: 2, MaxBackoffSeconds: 5}.Defaults()
	work, _, err := prepareIssueExecution(store, record.Run, policy)
	if err != nil || work == nil {
		t.Fatalf("prepare issue execution = %v, %v", work, err)
	}
	prURL := "https://github.com/acme/repo/pull/42"
	reset := time.Now().UTC().Add(10 * time.Minute)
	rate := &githubutil.RateLimitError{RetryAt: reset}
	if err := work.finish(t.Context(), localruntime.IssueChecksPending, app.Result{PRURL: prURL, Err: errors.Join(app.ErrChecksUnavailable, rate)}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(record.ID)
	if err != nil || got.Outcome != localruntime.IssueChecksPending || got.UnlockPending || !got.LabelAdded ||
		got.PRURL != prURL || !got.NextAttemptAt.Equal(reset) || got.Failures != record.Failures ||
		got.AgentAttempts != record.AgentAttempts || got.PRVerification != record.PRVerification {
		t.Fatalf("waiting issue receipt = %+v, %v", got, err)
	}
	if next, request, err := prepareIssueExecution(store, record.Run, policy); err != nil || next != nil || request != record.RequestID {
		t.Fatalf("early retry = %v, %q, %v", next, request, err)
	}
	got.NextAttemptAt = time.Now().Add(-time.Second)
	if err := store.Save(got); err != nil {
		t.Fatal(err)
	}
	next, _, err := prepareIssueExecution(store, record.Run, policy)
	if err != nil || next == nil {
		t.Fatalf("resume issue execution = %v, %v", next, err)
	}
	h := app.Harness{}
	ctx, done := next.configure(t.Context(), &h)
	defer done()
	if h.IssueChecksPRURL != prURL || !h.IssueCheckoutComplete || next.record.Run.Workflow.EventID != record.Run.Workflow.EventID ||
		next.record.RequestID != record.RequestID || next.record.ID != record.ID {
		t.Fatalf("retry lost issue, task, workflow or PR identity: %+v", next.record)
	}
	if err := next.finish(ctx, localruntime.IssueChecksPending, app.Result{Err: app.ErrChecksUnavailable}, nil); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load(record.ID)
	if err != nil || got.NextAttemptAt.Sub(got.StoppedAt) != 2*time.Second || got.Failures != 1 || got.UnlockPending ||
		got.AgentAttempts != record.AgentAttempts || got.PRURL != prURL {
		t.Fatalf("persistent failure did not use bounded backoff: %+v, %v", got, err)
	}
}

func TestIssueExecutionDiskFullRestoresAgentBudget(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	record.AgentAttempts = 1
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	work, _, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
	if err != nil || work == nil {
		t.Fatalf("prepare issue execution = %v, %v", work, err)
	}
	h := app.Harness{}
	ctx, done := work.configure(context.Background(), &h)
	defer done()
	h.OnStep("agent")
	running, err := store.Load(record.ID)
	if err != nil || running.AgentAttempts != 2 {
		t.Fatalf("running agent attempts = %d, error = %v", running.AgentAttempts, err)
	}
	if err := work.finish(ctx, "error", app.Result{ExitCode: app.ExitCodex, Err: syscall.ENOSPC}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(record.ID)
	if err != nil || got.AgentAttempts != 1 || got.Incident != localruntime.IssueIncidentWorkspaceStorage ||
		got.Outcome != "error" || got.WorkspaceDir != work.record.WorkspaceDir || !got.UnlockPending {
		t.Fatalf("disk exhaustion outcome = %+v, error = %v", got, err)
	}
}

func TestIssueExecutionStorageHoldSurvivesRestartWithoutTerminalReceipt(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	record.AgentAttempts = 1
	record.PriorRunFailures = 2
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	work, _, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
	if err != nil || work == nil {
		t.Fatalf("prepare issue execution = %v, %v", work, err)
	}
	defer work.lock.Close()
	h := app.Harness{}
	_, done := work.configure(t.Context(), &h)
	defer done()
	h.OnCheckout()
	h.OnStep("agent")
	running, err := store.Load(record.ID)
	if err != nil || running.AgentAttempts != 2 || !running.CheckoutComplete || running.Outcome != "" {
		t.Fatalf("running receipt = %+v, error = %v", running, err)
	}
	// Record the same fallback used when writing a terminal receipt fails.
	if err := store.ReleaseExecutionReserve(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := work.lock.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := &localruntime.IssueClaimStore{Root: store.Root}
	got, err := restarted.Load(record.ID)
	if err != nil || got.AgentAttempts != 1 || got.PriorRunFailures != 2 || !got.CheckoutComplete ||
		got.State != "blocked" || got.Incident != localruntime.IssueIncidentWorkspaceStorage ||
		got.Outcome != "error" || got.StoppedAt.IsZero() || !got.UnlockPending {
		t.Fatalf("restarted receipt = %+v, error = %v", got, err)
	}
	if work, duplicate, err := prepareIssueExecution(restarted, record.Run, localruntime.IssueRecoveryConfig{}); err != nil || work != nil || duplicate != record.RequestID {
		t.Fatalf("held work redispatched: work=%v duplicate=%q err=%v", work, duplicate, err)
	}
}

func TestIssueExecutionRetryReusesOnlyCompletedCheckout(t *testing.T) {
	t.Parallel()
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkout complete=%t", complete), func(t *testing.T) {
			t.Parallel()
			store, record := lifecycleRecord(t)
			previousDir := filepath.Join(filepath.Dir(store.Root), "issue-workspaces", strings.Repeat("b", 32))
			gitDir := filepath.Join(previousDir, "repo", ".git")
			if err := os.MkdirAll(gitDir, 0700); err != nil {
				t.Fatal(err)
			}
			record.WorkspaceDir = previousDir
			record.CheckoutComplete = complete
			if err := store.Save(record); err != nil {
				t.Fatal(err)
			}
			work, _, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
			if err != nil || work == nil {
				t.Fatalf("prepare issue execution = %v, %v", work, err)
			}
			defer work.lock.Close()
			want := previousDir
			if !complete {
				want = filepath.Join(filepath.Dir(previousDir), record.ID)
			}
			if work.record.WorkspaceDir != want {
				t.Fatalf("retry workspace = %q, want %q", work.record.WorkspaceDir, want)
			}
			if _, err := os.Stat(gitDir); err != nil {
				t.Fatalf("checkout evidence lost: %v", err)
			}
		})
	}
}

func TestIssueExecutionRejectsSupersededOwner(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	record.InstanceID = "previous-owner"
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	work, _, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
	if work != nil {
		work.lock.Close()
		t.Fatal("superseded owner was allowed to execute")
	}
	if !errors.Is(err, localruntime.ErrIssueOwnershipChanged) || strings.Contains(err.Error(), "%!") {
		t.Fatalf("unexpected ownership error: %v", err)
	}
}

func TestIssueExecutionTracksProcessExitAndStartFailure(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	work, _, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer work.lock.Close()
	runner := localruntime.IssueRunner{Runner: execx.OSRunner{}, Lock: work.lock, Store: store, RecordID: record.ID}
	for _, command := range []execx.Command{{Name: "sh", Args: []string{"-c", "exit 0"}}, {Name: filepath.Join(t.TempDir(), "missing-command")}} {
		_, _ = runner.Run(context.Background(), command)
		got, err := store.Load(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		stopped, err := localruntime.IssueProcessesStopped(got)
		if err != nil || !stopped || got.ProcessUncertain || len(got.ProcessGroups) != 0 {
			t.Fatalf("termination not recorded: %+v err=%v", got, err)
		}
	}
}

func TestIssueOperatorIntentSurvivesRestart(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	if err := persistIssueControl(store, record.RequestID, "paused"); err != nil {
		t.Fatal(err)
	}
	restarted := &localruntime.IssueClaimStore{Root: store.Root}
	if work, _, err := prepareIssueExecution(restarted, record.Run, localruntime.IssueRecoveryConfig{}); err == nil || work != nil {
		t.Fatal("paused issue automatically restarted")
	}
	if err := persistIssueControl(store, record.RequestID, ""); err != nil {
		t.Fatal(err)
	}
	work, _, err := prepareIssueExecution(restarted, record.Run, localruntime.IssueRecoveryConfig{})
	if err != nil || work == nil {
		t.Fatal(err)
	}
	work.lock.Close()
}

func TestIssueRepairOutboxDeduplicatesAndRetainsFailures(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	q := &issueRepairQueue{store: &localruntime.IssueRepairStore{Root: filepath.Join(t.TempDir(), "repairs")}, claims: store, logRoot: t.TempDir(), logf: func(string, ...any) {}}
	record.Step = "pr"
	record.Incident = "missing_pr"
	record.ExitCode = 0
	record.Error = "internal invariant failed"
	id, err := q.capture(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	again, err := q.capture(context.Background(), record)
	if err != nil || again != id {
		t.Fatal("identical incident not deduplicated")
	}
	record.RunError = "different invariant failed"
	record.ExitCode = app.ExitCodex
	record.Error = "agent retry budget exhausted"
	other, err := q.capture(context.Background(), record)
	if err != nil || other == id {
		t.Fatal("unrelated incident suppressed")
	}
	calls := 0
	q.reconcile(context.Background(), func(context.Context, config.Config, bool, string, bool) (string, error) {
		calls++
		return "", errors.New("agent auth required")
	})
	repairs, err := q.store.List()
	if err != nil || len(repairs) != 2 || calls != 2 {
		t.Fatalf("outbox=%v calls=%d err=%v", repairs, calls, err)
	}
	for _, repair := range repairs {
		if repair.State != "queued" || !repair.NextAttemptAt.After(time.Now()) {
			t.Fatal("failed enqueue was lost")
		}
		switch repair.RequestID {
		case id:
			if strings.Contains(repair.Run.Prompt, "exit_code=") {
				t.Fatal("legacy repair context reported an invented exit code")
			}
		case other:
			if !strings.Contains(repair.Run.Prompt, "exit_code=50") ||
				!strings.Contains(repair.Run.Prompt, "different invariant failed") ||
				strings.Contains(repair.Run.Prompt, "agent retry budget exhausted") {
				t.Fatal("repair context lost the terminal outcome")
			}
		}
	}
}

func TestIssueRetentionPreservesUnresolvedEvidence(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	repairs := &localruntime.IssueRepairStore{Root: filepath.Join(t.TempDir(), "repairs")}
	record.WorkspaceDir = filepath.Join(filepath.Dir(store.Root), "issue-workspaces", record.ID)
	if err := os.MkdirAll(record.WorkspaceDir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(record.WorkspaceDir, "pending.txt")
	if err := os.WriteFile(file, []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	logRoot := t.TempDir()
	if _, err := retainIssueEvidence(t.Context(), store, repairs, logRoot, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("unresolved work deleted")
	}
	record.State = "fulfilled"
	record.LabelAdded = false
	record.VerifiedAt = time.Now().Add(-31 * 24 * time.Hour)
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	lock, err := store.TryLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainIssueEvidence(t.Context(), store, repairs, logRoot, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("locked workspace deleted")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Concurrent subprocess tests can briefly inherit this descriptor between
	// fork and exec, keeping flock held after our close. Retention deliberately
	// skips busy locks and tries again on a later pass; exercise that contract.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := retainIssueEvidence(t.Context(), store, repairs, logRoot, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(file)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("resolved artifact not retired after repeated retention passes")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIssueRetentionAfterRetryWithBlockedRepair(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		started      bool
		remove       bool
	}{
		{name: "legacy rollout skip", reason: failurefollowup.ExternalRolloutPrerequisitesReason, remove: true},
		{name: "repair skipped after issue fulfillment", reason: issueRepairSourceFulfilledReason, remove: true},
		{name: "previously started rollout repair", reason: failurefollowup.ExternalRolloutPrerequisitesReason, started: true},
		{name: "other blocked repair", reason: localruntime.ErrIssueOwnershipChanged.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			repairs := &localruntime.IssueRepairStore{Root: t.TempDir()}
			id := strings.Repeat("b", 32)
			repair := localruntime.IssueRepairRecord{ID: id, RequestID: "repair-" + id, State: "blocked", Error: tc.reason, Started: tc.started}
			if err := repairs.Save(repair); err != nil {
				t.Fatal(err)
			}
			issue.State = "incident"
			issue.Outcome = "error"
			issue.LabelAdded = false
			issue.RepairRequestID = repair.RequestID
			issue.WorkspaceDir = filepath.Join(filepath.Dir(claims.Root), "issue-workspaces", issue.ID)
			logRoot := t.TempDir()
			logDir, ok := taskLogDir(logRoot, issue.RequestID)
			if !ok {
				t.Fatal("invalid issue log path")
			}
			paths := []string{issue.WorkspaceDir, logDir}
			for _, path := range paths {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(logDir, logFileName)
			if err := os.WriteFile(logPath, []byte("task evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := claims.Save(issue); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			checkRetention := func(now time.Time, removeWorkspace, removeLogs bool) {
				t.Helper()
				if _, err := retainIssueEvidence(t.Context(), claims, repairs, logRoot, nil, now); err != nil {
					t.Fatal(err)
				}
				for path, remove := range map[string]bool{issue.WorkspaceDir: removeWorkspace, logPath: removeLogs} {
					_, err := os.Stat(path)
					if (remove && !errors.Is(err, os.ErrNotExist)) || (!remove && err != nil) {
						t.Errorf("stat %s after retention = %v, want removed=%t", path, err, remove)
					}
				}
			}
			checkRetention(now, false, false)
			retry := issue
			retry.ID = strings.Repeat("c", 32)
			retry.RequestID = "issue-" + retry.ID
			claim := *issue.Run.GitHubIssue.Claim
			claim.ID = retry.ID
			claim.Attempt++
			issueCfg := *issue.Run.GitHubIssue
			issueCfg.Claim = &claim
			retry.Run.GitHubIssue = &issueCfg
			retry.State = "fulfilled"
			retry.VerifiedAt = now
			retry.RepairRequestID = ""
			retry.WorkspaceDir = ""
			if err := claims.Save(retry); err != nil {
				t.Fatal(err)
			}
			checkRetention(now.Add(29*24*time.Hour), tc.remove, false)
			checkRetention(now.Add(31*24*time.Hour), tc.remove, tc.remove)
		})
	}
}

type repairPRRunner func(context.Context, execx.Command) (execx.Result, error)

func (f repairPRRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	return f(ctx, cmd)
}

func TestIssueRepairRequiresVerifiedPR(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state, merged, head string
		valid               bool
	}{{"OPEN", "null", "repair-branch", true}, {"CLOSED", "null", "repair-branch", false}, {"MERGED", `"2026-01-01T00:00:00Z"`, "repair-branch", true}, {"OPEN", "null", "unrelated", false}} {
		q := &issueRepairQueue{runner: repairPRRunner(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("unbounded PR verification")
			}
			return execx.Result{Stdout: `{"url":"https://github.com/acme/harness/pull/42","state":"` + tc.state + `","mergedAt":` + tc.merged + `,"headRefName":"` + tc.head + `"}`}, nil
		})}
		record := localruntime.IssueRepairRecord{PRURL: "https://github.com/acme/harness/pull/42", Branch: "repair-branch", Run: config.Config{RepoURL: "https://github.com/acme/harness"}}
		if err := q.verifyPR(record); (err == nil) != tc.valid {
			t.Fatalf("state=%s head=%s verified=%v", tc.state, tc.head, err)
		}
	}
}

func TestIssueRepairVerifiesSSHRepositoryIdentity(t *testing.T) {
	t.Parallel()
	for _, repo := range []string{"git@github.com:acme/harness.git", "ssh://git@github.com/acme/harness.git", "https://github.com/acme/harness.git", "acme/harness"} {
		t.Run(repo, func(t *testing.T) {
			calls := 0
			q := issueRepairQueue{runner: repairPRRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				calls++
				if !strings.Contains(strings.Join(cmd.Args, " "), "--repo acme/harness") {
					t.Fatalf("wrong verification target: %v", cmd.Args)
				}
				return execx.Result{Stdout: `{"url":"https://github.com/acme/harness/pull/42","state":"OPEN","headRefName":"repair-branch"}`}, nil
			})}
			record := localruntime.IssueRepairRecord{PRURL: "https://github.com/acme/harness/pull/42", Branch: "repair-branch", Run: config.Config{RepoURL: repo}}
			if err := q.verifyPR(record); err != nil || calls != 1 {
				t.Fatalf("verification = %v, calls = %d", err, calls)
			}
			record.PRURL = "https://github.com/other/harness/pull/42"
			if err := q.verifyPR(record); err == nil || calls != 1 {
				t.Fatal("cross-repository repair was accepted")
			}
		})
	}
}

func TestIssueRepairOutboxBlocksLegacyOwnershipFailure(t *testing.T) {
	t.Parallel()
	claims, issue := lifecycleRecord(t)
	q := &issueRepairQueue{store: &localruntime.IssueRepairStore{Root: filepath.Join(t.TempDir(), "repairs")}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
	for _, state := range []string{"queued", "running"} {
		t.Run(state, func(t *testing.T) {
			// An older binary stored the original error only in the prompt.
			failure := errors.New("submit claimed issue: issue owner is unavailable or changed: %!w(<nil>)")
			run := failureFollowUpRunConfigWithContext(issue.RequestID, app.Result{}, issue.Run, q.logRoot, formatFailureFollowUpContext("0", failure, issue.Run.Prompt))
			id := strings.Repeat("b", 32)
			old := localruntime.IssueRepairRecord{ID: id, RequestID: "repair-" + id, State: state, Run: run}
			if err := q.store.Save(old); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				q.reconcile(context.Background(), func(context.Context, config.Config, bool, string, bool) (string, error) {
					t.Error("legacy ownership repair dispatched")
					return "", errors.New("unexpected dispatch")
				})
			}
			repairs, err := q.store.List()
			if err != nil || len(repairs) != 1 {
				t.Fatalf("expected one retained repair, got %d, error=%v", len(repairs), err)
			}
			saved := repairs[0]
			if saved.State != "blocked" || saved.Error == "" || saved.Started || saved.Run.Prompt != old.Run.Prompt {
				t.Fatalf("legacy repair state=%s error=%q started=%t prompt retained=%t", saved.State, saved.Error, saved.Started, saved.Run.Prompt == old.Run.Prompt)
			}
		})
	}
}

func TestIssueRepairOutboxRechecksLegacyRolloutFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, state, exitCode, details string
		blocked                        bool
	}{
		{"queued and linked", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding.", true},
		{"dispatch intent without a claim link", "running", "", "IAM approval and Secrets Manager provisioning remain outstanding.", true},
		{"joined provisioning and flag status", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding | API and MCP flags default to off.", true},
		{"provisioning with a nonbreaking space after the period", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding.\u00a0API flags default to off.", true},
		{"provisioning with an ellipsis", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding... API flags default to off.", true},
		{"provisioning with an unspaced ellipsis", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding...API flags default to off.", true},
		{"provisioning with a Unicode ellipsis", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding… API flags default to off…", true},
		{"external provisioning with public probe evidence", "queued", "50", "Production ECS and Vault access require external provisioning. Public MCP health returned HTTP 200, and unauthenticated initialization returned HTTP 401. Production worker connectivity remains unverified.", true},
		{"provisioning with unavailable integration validation", "queued", "50", "Scoped IAM, shared reference key, and deployment still require external provisioning. API flags default off. Docker-backed integration tests could not run because no container runtime was available.", true},
		{"uncorroborated legacy rollout prerequisites", "queued", "50", "Infrastructure changes, shared Secrets Manager key, and live verification remain pending. API and MCP evidence switches default to off.", false},
		{"legacy rollout with only secret confirmed", "queued", "50", "Infrastructure changes, shared Secrets Manager key, and live verification remain pending. API and MCP evidence switches default to off. The shared Secrets Manager key requires external provisioning.", false},
		{"corroborated legacy rollout prerequisites", "queued", "50", "Infrastructure changes, shared Secrets Manager key, and live verification remain pending. API and MCP evidence switches default to off. Infrastructure changes and the shared Secrets Manager key require external provisioning.", true},
		{"infrastructure configuration work", "queued", "50", "IAM policies and Secrets Manager configuration still require infrastructure work.", false},
		{"ambiguous IAM and secret configuration", "queued", "50", "IAM setup and Secrets Manager configuration are outstanding.", false},
		{"actionable", "queued", "50", "IAM approval and Secrets Manager provisioning are complete, but local fixes remain outstanding.", false},
		{"actionable after provisioning details", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding. Deployment tests fail.", false},
		{"actionable after provisioning with an ellipsis and nonbreaking space", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding...\u00a0Deployment tests fail.", false},
		{"actionable flag regression", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding | Due to a regression API flags default to off.", false},
		{"stale stored blocker", "blocked", "50", "IAM setup and Secrets Manager configuration are outstanding.", false},
		{"confirmed stored blocker", "blocked", "50", "IAM setup and Secrets Manager configuration require external provisioning.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			q := &issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
			failure := errors.New("codex: codex reported failure: Failure: Production rollout remains pending.\nError details: " + tc.details)
			run := failureFollowUpRunConfigWithContext(issue.RequestID, app.Result{}, issue.Run, q.logRoot, formatFailureFollowUpContext(tc.exitCode, failure, issue.Run.Prompt))
			run.Prompt = legacyRepairPrompt(run.Prompt, failure.Error())
			id := strings.Repeat("b", 32)
			record := localruntime.IssueRepairRecord{ID: id, RequestID: "repair-" + id, State: tc.state, Run: run}
			if tc.state == "queued" || tc.state == "blocked" {
				issue.Outcome = "error"
				issue.Error = failure.Error()
				issue.Incident = "missing_pr"
				issue.RepairRequestID = record.RequestID
				if tc.state == "blocked" {
					record.Error = failurefollowup.ExternalRolloutPrerequisitesReason
					issue.Incident = localruntime.IssueIncidentExternalRollout
				}
				if err := claims.Save(issue); err != nil {
					t.Fatal(err)
				}
			}
			if err := q.store.Save(record); err != nil {
				t.Fatal(err)
			}
			calls := 0
			for range 2 {
				q.reconcile(context.Background(), func(context.Context, config.Config, bool, string, bool) (string, error) {
					calls++
					return "", errors.New("agent auth required")
				})
			}
			repairs, err := q.store.List()
			if err != nil || len(repairs) != 1 {
				t.Fatalf("expected one retained repair, got %d, error=%v", len(repairs), err)
			}
			got := repairs[0]
			if tc.blocked {
				if calls != 0 || got.State != "blocked" || got.Error != failurefollowup.ExternalRolloutPrerequisitesReason || got.Started {
					t.Fatalf("external prerequisite: calls=%d state=%q error=%q started=%t", calls, got.State, got.Error, got.Started)
				}
			} else if calls != 1 || got.State != "queued" || !got.NextAttemptAt.After(time.Now()) {
				t.Fatalf("actionable failure: calls=%d state=%q next attempt=%v", calls, got.State, got.NextAttemptAt)
			}
		})
	}
}

func TestIssueRepairOutboxRechecksTruncatedRolloutFromStdout(t *testing.T) {
	t.Parallel()
	const summary = "Failure: #42 production rollout remains incomplete."
	const prerequisites = "Infrastructure changes, shared Secrets Manager key, and live verification still require external provisioning. API and MCP evidence switches default to off."
	const progress = "Added the endpoint in /tmp/worktree."
	const stored = "codex: codex reported failure: " + summary + " Error details: " + prerequisites +
		" | " + progress + " | Updated fixtures. | Validation passed for unit tests. | [error details truncated]"
	for _, tc := range []struct {
		name, state, file, stream, lastLine string
		blocked, sourceAlreadyBlocked       bool
	}{
		{"linked repair with full report", "queued", logFileName, "stdout", "API flags default off.", true, false},
		{"dispatch intent with legacy log", "running", legacyTaskLogFileName, "stdout", "API flags default off.", true, false},
		{"deployment defect beyond truncation", "queued", logFileName, "stdout", "Smoke check returned HTTP 500.", false, false},
		{"stored blocker with deployment defect beyond truncation", "blocked", logFileName, "stdout", "Smoke check returned HTTP 500.", false, true},
		{"stored blocker with missing stdout", "blocked", "", "", "", true, true},
		{"stored repair blocker with source still eligible for retry", "blocked", logFileName, "stdout", "API flags default off.", true, false},
		{"readiness timeout under validation beyond truncation", "queued", logFileName, "stdout", "## Validation\n- Readiness check timed out.", false, false},
		{"passing validation beyond truncation", "queued", logFileName, "stdout", "## Validation\n- Unit tests passed.", true, false},
		{"stderr echo", "queued", logFileName, "stderr", "API flags default off.", false, false},
		{"unrelated report", "queued", logFileName, "stdout", "API flags default off.", false, false},
		{"missing log", "queued", "", "", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			q := &issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
			issue.RunError = stored
			issue.ExitCode = app.ExitCodex
			record := saveLegacyIssueRepair(t, q, issue)
			record.State = tc.state
			if tc.state == "blocked" {
				record.Error = failurefollowup.ExternalRolloutPrerequisitesReason
			}
			if err := q.store.Save(record); err != nil {
				t.Fatal(err)
			}
			issue.Outcome = "error"
			issue.Incident = "missing_pr"
			issue.StoppedAt = time.Now().Add(-time.Minute)
			issue.NextAttemptAt = time.Now().Add(time.Minute)
			if tc.sourceAlreadyBlocked {
				issue.Incident = localruntime.IssueIncidentExternalRollout
				issue.NextAttemptAt = time.Time{}
			}
			if tc.state == "queued" || tc.state == "blocked" {
				issue.RepairRequestID = record.RequestID
			} else {
				issue.RunError = ""
			}
			if err := claims.Save(issue); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				dir, _ := taskLogDir(q.logRoot, issue.RequestID)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				added := progress
				if tc.name == "unrelated report" {
					added = "Added the worker."
				}
				var log strings.Builder
				for _, line := range []string{summary, "Error details: " + prerequisites, added, "Updated fixtures.",
					"Validation passed for unit tests.", tc.lastLine} {
					fmt.Fprintf(&log, "task request_id=%s cmd phase=codex name=codex stream=%s b64=%s\n",
						issue.RequestID, tc.stream, base64.StdEncoding.EncodeToString([]byte(line)))
				}
				if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(log.String()), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			enqueue := func(context.Context, config.Config, bool, string, bool) (string, error) {
				calls++
				return "", errors.New("agent auth required")
			}
			if tc.blocked {
				lock, err := claims.TryLock(localruntime.IssueExecutionKey(issue.Run.GitHubIssue))
				if err != nil || lock == nil {
					t.Fatalf("lock source claim: %v", err)
				}
				q.reconcile(t.Context(), enqueue)
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				repairs, err := q.store.List()
				if err != nil || len(repairs) != 1 || repairs[0].State != tc.state || calls != 0 {
					t.Fatalf("changed outbox or dispatched while source claim was locked: calls=%d err=%v", calls, err)
				}
				source, err := claims.Load(issue.ID)
				if err != nil {
					t.Fatal(err)
				}
				if source.Incident != issue.Incident || !source.NextAttemptAt.Equal(issue.NextAttemptAt) {
					t.Fatalf("changed source claim while locked: incident=%q next attempt=%v", source.Incident, source.NextAttemptAt)
				}
			}
			// A concurrent subprocess can retain a descriptor until exec.
			// Reconciliation retries busy locks on the next pass.
			waitForCondition(t, 5*time.Second, func() bool {
				q.reconcile(t.Context(), enqueue)
				if !tc.blocked {
					return calls > 0
				}
				if calls != 0 {
					t.Fatal("dispatched repair while recovering an external rollout blocker")
				}
				repairs, err := q.store.List()
				if err != nil || len(repairs) != 1 {
					t.Fatalf("expected one retained repair, got %d, error=%v", len(repairs), err)
				}
				source, err := claims.Load(issue.ID)
				if err != nil {
					t.Fatal(err)
				}
				return repairs[0].State == "blocked" && source.Incident == localruntime.IssueIncidentExternalRollout
			})
			q.reconcile(t.Context(), enqueue)
			repairs, err := q.store.List()
			if err != nil || len(repairs) != 1 {
				t.Fatalf("expected one retained repair, got %d, error=%v", len(repairs), err)
			}
			got := repairs[0]
			source, err := claims.Load(issue.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.blocked {
				if calls != 0 || got.State != "blocked" || got.Error != failurefollowup.ExternalRolloutPrerequisitesReason || got.Started {
					t.Fatalf("external prerequisite: calls=%d state=%q error=%q started=%t", calls, got.State, got.Error, got.Started)
				}
				if source.Incident != localruntime.IssueIncidentExternalRollout || !source.NextAttemptAt.IsZero() ||
					source.RunError != issue.RunError || source.RepairRequestID != issue.RepairRequestID {
					t.Fatalf("source claim lost retry blocker or evidence: incident=%q next attempt=%v", source.Incident, source.NextAttemptAt)
				}
			} else if calls != 1 || got.State != "queued" {
				t.Fatalf("actionable failure: calls=%d state=%q", calls, got.State)
			} else if source.Incident != issue.Incident || !source.NextAttemptAt.Equal(issue.NextAttemptAt) {
				t.Fatalf("actionable source claim changed: incident=%q next attempt=%v", source.Incident, source.NextAttemptAt)
			}
		})
	}
}

func TestIssueRepairOutboxBlocksTruncatedExternalEvidence(t *testing.T) {
	t.Parallel()
	claims, issue := lifecycleRecord(t)
	q := &issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
	const summary = "Failure: Equipment audit remains blocked."
	const details = "Error details: This runtime had no approved inspection record or authorized access to matching equipment logs."
	lines := []string{summary, details, "| Item | Evidence needed |", "|---|---|", "| Pump | Signed inspection record |", "| Motor | Maintenance log |"}
	issue.RunError = "codex: codex reported failure: " + summary + " " + details + " | " + strings.Join(lines[2:5], " | ") + failurefollowup.TruncatedErrorDetails
	issue.Outcome = "error"
	issue.Incident = "missing_pr"
	issue.NextAttemptAt = time.Now().Add(time.Minute)
	repair := saveLegacyIssueRepair(t, q, issue)
	issue.RepairRequestID = repair.RequestID
	if err := claims.Save(issue); err != nil {
		t.Fatal(err)
	}
	dir, _ := taskLogDir(q.logRoot, issue.RequestID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&log, "task request_id=%s cmd phase=codex name=codex stream=stdout b64=%s\n",
			issue.RequestID, base64.StdEncoding.EncodeToString([]byte(line)))
	}
	if err := os.WriteFile(filepath.Join(dir, logFileName), []byte(log.String()), 0600); err != nil {
		t.Fatal(err)
	}
	q.reconcile(t.Context(), func(context.Context, config.Config, bool, string, bool) (string, error) {
		t.Fatal("missing external evidence dispatched a repair")
		return "", nil
	})
	repairs, err := q.store.List()
	if err != nil || len(repairs) != 1 || repairs[0].State != "blocked" || repairs[0].Error != failurefollowup.ExternalEvidencePrerequisitesReason {
		t.Fatalf("expected blocked repair, got %v, error=%v", repairs, err)
	}
	saved, err := claims.Load(issue.ID)
	if err != nil || saved.Incident != localruntime.IssueIncidentExternalEvidence || !saved.NextAttemptAt.IsZero() {
		t.Fatalf("expected persisted prerequisite and cleared retry deadline, got %q and %v, error=%v", saved.Incident, saved.NextAttemptAt, err)
	}
}

func TestIssueRepairRecoversRawAgentStdoutAtEveryLogLevel(t *testing.T) {
	t.Parallel()
	for _, level := range []localruntime.LogLevel{localruntime.LogLevelDebug, localruntime.LogLevelInfo, localruntime.LogLevelWarn} {
		for _, stage := range []string{"codex", "claude"} {
			t.Run(level.String()+"/"+stage, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), ".log")
				mirror, err := newTaskLogMirror(root)
				if err != nil {
					t.Fatal(err)
				}
				var rendered strings.Builder
				logger := newTerminalLogger(&rendered, false)
				logger.sink = mirror
				router := newRuntimeLogRouter(logger, nil, level)
				const requestID = "local-1712345678-000007"
				// A long line exercises terminal truncation while structured output can be
				// filtered from the display entirely. Both must survive in the task mirror.
				lines := []string{"Failure: Production rollout remains pending.", strings.Repeat("diagnostic ", 1000), `{"type":"result","result":"complete"}`}
				for _, line := range lines {
					router.Printf("task request_id=%s cmd phase=%s name=%s stream=stdout b64=%s", requestID, stage, stage, base64.StdEncoding.EncodeToString([]byte(line)))
				}
				logger.Close()
				if got, want := issueRepairAgentStdout(root, requestID, stage), strings.Join(lines, "\n")+"\n"; got != want {
					t.Fatalf("recovered stdout length=%d, want %d", len(got), len(want))
				}
				if strings.Contains(rendered.String(), "b64=") {
					t.Fatal("raw payload leaked into rendered output")
				}
			})
		}
	}
}
