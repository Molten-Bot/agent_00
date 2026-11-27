package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestIssueRetentionBothLayouts(t *testing.T) {
	t.Parallel()
	store, fulfilled := lifecycleRecord(t)
	repairs := &localruntime.IssueRepairStore{Root: t.TempDir()}
	base := filepath.Dir(store.Root)
	queue := filepath.Join(base, "issue-queue")
	legacy := &localruntime.IssueClaimStore{Root: filepath.Join(queue, "issue-claims")}
	roots := []string{filepath.Join(base, "issue-workspaces"), filepath.Join(queue, "issue-workspaces")}
	logRoot := filepath.Join(base, "logs", ".log")
	now := time.Now()

	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("evidence"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(path string, present bool) {
		t.Helper()
		_, err := os.Stat(path)
		if (present && err != nil) || (!present && !errors.Is(err, os.ErrNotExist)) {
			t.Errorf("stat %s = %v, want present=%t", path, err, present)
		}
	}
	claim := func(ch string, number int, state, outcome, control string) localruntime.IssueClaimRecord {
		r := fulfilled
		r.ID = strings.Repeat(ch, 32)
		r.RequestID = "issue-" + r.ID
		r.State, r.Outcome, r.Control = state, outcome, control
		issue, metadata := *r.Run.GitHubIssue, *r.Run.GitHubIssue.Claim
		metadata.ID, issue.Number = r.ID, number
		issue.Claim, r.Run.GitHubIssue = &metadata, &issue
		r.LabelAdded = false
		r.VerifiedAt = now
		return r
	}
	cache := func(root, id string) string {
		return filepath.Join(root, id, "repo", "cache")
	}

	fulfilled = claim("a", 1, "fulfilled", "completed", "")
	failed := claim("b", 2, "blocked", "error", "")
	stopped := claim("c", 3, "blocked", "stopped", "stopped")
	retrying := claim("d", 4, "retry_wait", "error", "")
	review := claim("e", 5, "fulfilled", "completed", "")
	verifying := claim("f", 6, "fulfilled", "completed", "")
	verifying.PRVerification = "pending verifier"
	stoppedSuccessor := claim("1", 3, "fulfilled", "completed", "")
	stoppedSuccessor.Run.GitHubIssue.Claim.Attempt++
	active := claim("2", 7, "running", "", "")
	active.WorkspaceDir = filepath.Join(roots[0], fulfilled.ID)
	unresolvedStop := claim("3", 8, "blocked", "stopped", "stopped")
	unknown := strings.Repeat("9", 32)
	for _, r := range []localruntime.IssueClaimRecord{fulfilled, failed, stopped, stoppedSuccessor, review, verifying} {
		if err := store.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []localruntime.IssueClaimRecord{retrying, active, unresolvedStop} {
		if err := legacy.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range roots {
		for _, id := range []string{fulfilled.ID, failed.ID, stopped.ID, retrying.ID, review.ID, verifying.ID, active.ID, unresolvedStop.ID, unknown} {
			write(cache(root, id))
		}
	}
	if err := workflow.SaveReviewOutcome(base, review.RequestID, workflow.Outcome{Status: workflow.OutcomeReviewPending}); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(roots[1], stopped.ID, "review-pass.receipt.json")
	write(receipt)
	logDir, _ := taskLogDir(filepath.Join(queue, "logs", ".log"), stopped.RequestID)
	log := filepath.Join(logDir, legacyTaskLogFileName)
	write(log)
	archive := workflow.ReviewOutcomePath(base, stopped.RequestID)
	if err := workflow.SaveReviewOutcome(base, stopped.RequestID, workflow.Outcome{Status: "failed"}); err != nil {
		t.Fatal(err)
	}

	sweep := func(at time.Time) issueRetentionStats {
		t.Helper()
		stats, err := retainIssueEvidence(t.Context(), store, repairs, logRoot, nil, at)
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	if stats := sweep(now); stats.reclaimedBytes == 0 || stats.protected != 16 {
		t.Errorf("unexpected cleanup summary %+v", stats)
	}
	for _, root := range roots {
		check(cache(root, stopped.ID), false)
		for _, id := range []string{fulfilled.ID, failed.ID, retrying.ID, review.ID, verifying.ID, active.ID, unresolvedStop.ID, unknown} {
			check(cache(root, id), true)
		}
	}
	check(receipt, true)
	check(log, true)
	check(archive, true)
	sweep(now.Add(issueEvidenceRetention))
	check(receipt, false)
	check(log, false)
	check(archive, false)
	if r, err := store.Load(stopped.ID); err != nil || r.Outcome != "stopped" {
		t.Fatalf("stopped audit record changed: outcome=%q err=%v", r.Outcome, err)
	}
	if stats := sweep(now.Add(issueEvidenceRetention)); stats.reclaimedBytes != 0 {
		t.Errorf("repeat cleanup reclaimed bytes %+v", stats)
	}
}

func TestIssueRetentionRetriesUnsafeRoot(t *testing.T) {
	t.Parallel()
	store, record := lifecycleRecord(t)
	repairs := &localruntime.IssueRepairStore{Root: t.TempDir()}
	base := filepath.Dir(store.Root)
	workspaceRoot := filepath.Join(base, "issue-workspaces")
	external := t.TempDir()
	cache := filepath.Join(external, record.ID, "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, workspaceRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	record.State, record.Outcome, record.LabelAdded = "fulfilled", "completed", false
	record.VerifiedAt = time.Now()
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	sweep := func() error {
		_, err := retainIssueEvidence(t.Context(), store, repairs, "", nil, time.Now())
		return err
	}
	if err := sweep(); err == nil {
		t.Fatal("expected cleanup error for symlink workspace root")
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(external, workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if err := sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, record.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup retry left workspace: %v", err)
	}
	if r, err := store.Load(record.ID); err != nil || !r.Fulfilled() {
		t.Fatalf("maintenance changed delivery result: state=%q err=%v", r.State, err)
	}
}
