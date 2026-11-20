package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

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
	if err := work.finish(context.Background(), "error", app.Result{Err: errors.New("push failed")}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UnlockPending || got.StoppedAt.IsZero() || got.Outcome != "error" || got.Error != "push failed" || got.WorkspaceDir == "" {
		t.Fatalf("lost terminal evidence: %+v", got)
	}
	if work2, duplicate, err := prepareIssueExecution(store, record.Run, localruntime.IssueRecoveryConfig{}); err != nil || work2 != nil || duplicate != record.RequestID {
		t.Fatal("completed attempt dispatched again")
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
	if err == nil || !strings.Contains(err.Error(), "issue ownership changed") || strings.Contains(err.Error(), "%!") {
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
	record.Error = "internal invariant failed"
	id, err := q.capture(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	again, err := q.capture(context.Background(), record)
	if err != nil || again != id {
		t.Fatal("identical incident not deduplicated")
	}
	record.Error = "different invariant failed"
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
	if err := retainIssueEvidence(store, repairs, logRoot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("unresolved work deleted")
	}
	record.State = "fulfilled"
	record.VerifiedAt = time.Now().Add(-31 * 24 * time.Hour)
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	lock, err := store.TryLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
	if err != nil {
		t.Fatal(err)
	}
	if err := retainIssueEvidence(store, repairs, logRoot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("locked workspace deleted")
	}
	lock.Close()
	if err := retainIssueEvidence(store, repairs, logRoot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resolved artifact not retired")
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
