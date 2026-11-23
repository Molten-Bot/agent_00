package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
)

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
	if err := work.finish(ctx, outcome.State, outcome.Result); err != nil {
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
	if err := work.finish(context.Background(), "error", app.Result{ExitCode: app.ExitGit, Err: errors.New("push failed")}); err != nil {
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
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Concurrent subprocess tests can briefly inherit this descriptor between
	// fork and exec, keeping flock held after our close. Retention deliberately
	// skips busy locks and tries again on a later pass; exercise that contract.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := retainIssueEvidence(store, repairs, logRoot, time.Now()); err != nil {
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
			if err := claims.Save(issue); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			checkRetention := func(now time.Time, remove bool) {
				t.Helper()
				if err := retainIssueEvidence(claims, repairs, logRoot, now); err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					_, err := os.Stat(path)
					if (remove && !errors.Is(err, os.ErrNotExist)) || (!remove && err != nil) {
						t.Errorf("stat %s after retention = %v, want removed=%t", path, err, remove)
					}
				}
			}
			checkRetention(now, false)
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
			checkRetention(now.Add(29*24*time.Hour), false)
			checkRetention(now.Add(31*24*time.Hour), tc.remove)
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
		{"provisioning with unavailable integration validation", "queued", "50", "Scoped IAM, shared reference key, and deployment still require infrastructure work. API flags default off. Docker-backed integration tests could not run because no container runtime was available.", true},
		{"actionable", "queued", "50", "IAM approval and Secrets Manager provisioning are complete, but local fixes remain outstanding.", false},
		{"actionable after provisioning details", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding. Deployment tests fail.", false},
		{"actionable flag regression", "queued", "50", "IAM approval and Secrets Manager provisioning remain outstanding | Due to a regression API flags default to off.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			q := &issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
			failure := errors.New("codex: codex reported failure: Failure: Production rollout remains pending.\nError details: " + tc.details)
			run := failureFollowUpRunConfigWithContext(issue.RequestID, app.Result{}, issue.Run, q.logRoot, formatFailureFollowUpContext(tc.exitCode, failure, issue.Run.Prompt))
			id := strings.Repeat("b", 32)
			record := localruntime.IssueRepairRecord{ID: id, RequestID: "repair-" + id, State: tc.state, Run: run}
			if tc.state == "queued" {
				issue.Outcome = "error"
				issue.Error = failure.Error()
				issue.Incident = "missing_pr"
				issue.RepairRequestID = record.RequestID
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
	const summary = "Failure: Production rollout remains pending."
	const prerequisites = "IAM approval and Secrets Manager provisioning remain outstanding."
	const progress = "Added the endpoint in /tmp/worktree."
	const stored = "codex: codex reported failure: " + summary + " Error details: " + prerequisites +
		" | " + progress + " | Updated fixtures. | Validation passed for unit tests. | [error details truncated]"
	for _, tc := range []struct {
		name, state, file, stream, lastLine string
		blocked                             bool
	}{
		{"linked repair with full report", "queued", logFileName, "stdout", "API flags default off.", true},
		{"dispatch intent with legacy log", "running", legacyTaskLogFileName, "stdout", "API flags default off.", true},
		{"deployment defect beyond truncation", "queued", logFileName, "stdout", "Smoke check returned HTTP 500.", false},
		{"readiness timeout under validation beyond truncation", "queued", logFileName, "stdout", "## Validation\n- Readiness check timed out.", false},
		{"passing validation beyond truncation", "queued", logFileName, "stdout", "## Validation\n- Unit tests passed.", true},
		{"stderr echo", "queued", logFileName, "stderr", "API flags default off.", false},
		{"unrelated report", "queued", logFileName, "stdout", "API flags default off.", false},
		{"missing log", "queued", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			q := &issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
			issue.RunError = stored
			issue.ExitCode = app.ExitCodex
			record := saveLegacyIssueRepair(t, q, issue)
			record.State = tc.state
			if err := q.store.Save(record); err != nil {
				t.Fatal(err)
			}
			issue.Outcome = "error"
			issue.Incident = "missing_pr"
			issue.StoppedAt = time.Now().Add(-time.Minute)
			issue.NextAttemptAt = time.Now().Add(time.Minute)
			if tc.state == "queued" {
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
				lock.Close()
				repairs, err := q.store.List()
				if err != nil || len(repairs) != 1 || repairs[0].State != tc.state || calls != 0 {
					t.Fatalf("changed outbox or dispatched while source claim was locked: calls=%d err=%v", calls, err)
				}
			}
			for range 2 {
				q.reconcile(t.Context(), enqueue)
			}
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
