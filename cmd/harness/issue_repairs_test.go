package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestIssueRepairCaptureUsesWorkerFailureAfterUnlockError(t *testing.T) {
	t.Parallel()
	q := issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, logf: t.Logf, wake: make(chan struct{}, 1)}
	issue := localruntime.IssueClaimRecord{
		Step: "agent", Incident: "missing_pr", Outcome: "error", ExitCode: app.ExitCodex,
		Error: "temporary label removal failure", RunError: "worker failed in /private/workspace",
	}
	id, err := q.capture(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-q.wake:
	default:
		t.Fatal("new durable repair did not wake the queue")
	}
	issue.Error = "different unlock failure"
	if retryID, err := q.capture(context.Background(), issue); err != nil || retryID != id {
		t.Fatalf("same worker failure queued a different repair: %q, %v", retryID, err)
	}
	select {
	case <-q.wake:
		t.Fatal("duplicate capture woke the queue again")
	default:
	}
	records, err := q.store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("repair records = %d, err = %v", len(records), err)
	}
	prompt := records[0].Run.Prompt
	want := fmt.Sprintf("- exit_code=%d\n- error=%q", issue.ExitCode, "worker failed in [redacted-path]")
	if !strings.Contains(prompt, want) || strings.Contains(prompt, "label removal") || strings.Contains(prompt, "/private/workspace") {
		t.Fatal("repair prompt did not pair the exit code with the redacted worker failure")
	}
}

func TestIssueRepairOutboxRechecksDependencyBlockers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, captured, current, incident, want string
		missingReceipt, missingLogRoot, legacy  bool
		changedLogRoot                          bool
	}{
		{
			name:     "legacy queued report",
			captured: "codex: codex reported failure: Failure: Export remains blocked. Error details: #72 is still open and blocks the export | Verification is pending.",
			want:     failurefollowup.OpenDependencyReason,
		},
		{
			name:     "source report before classification",
			captured: "temporary recovery failure",
			current:  "codex: codex reported failure: Failure: Export remains blocked. Error details: [#72](https://github.com/example/project/issues/72) is still open and blocks the export.",
			want:     failurefollowup.OpenDependencyReason,
		},
		{
			name:     "persisted dependency incident",
			captured: "temporary recovery failure",
			incident: localruntime.IssueIncidentUnmergedDependency,
			want:     failurefollowup.UnmergedDependencyReason,
		},
		{
			name:           "redacted issue link without receipt",
			captured:       "codex: codex reported failure: Failure: Export remains blocked. Error details: [#72](https://github.com/example/project/issues/72) is still open and blocks the export.",
			want:           failurefollowup.OpenDependencyReason,
			missingReceipt: true,
		},
		{
			name:           "source classified after capture without receipt",
			captured:       "temporary recovery failure",
			incident:       localruntime.IssueIncidentOpenDependency,
			want:           failurefollowup.OpenDependencyReason,
			missingReceipt: true,
		},
		{
			name:           "legacy redacted issue link without receipt or log path",
			captured:       "codex: codex reported failure: Failure: Export remains blocked. Error details: [#72](https://github.com/example/project/issues/72) is still open and blocks the export.",
			want:           failurefollowup.OpenDependencyReason,
			missingReceipt: true,
			missingLogRoot: true,
			legacy:         true,
		},
		{
			name:           "legacy source reclassified without receipt after log root changes",
			captured:       "temporary recovery failure",
			current:        "upstream changes are pending",
			incident:       localruntime.IssueIncidentOpenDependency,
			want:           failurefollowup.OpenDependencyReason,
			missingReceipt: true,
			legacy:         true,
			changedLogRoot: true,
		},
		{
			name:           "source reclassified without receipt or log path",
			captured:       "temporary recovery failure",
			current:        "upstream changes are pending",
			incident:       localruntime.IssueIncidentOpenDependency,
			want:           failurefollowup.OpenDependencyReason,
			missingReceipt: true,
			missingLogRoot: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			q := issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
			if tc.missingLogRoot {
				q.logRoot = ""
			}
			issue.Incident = "missing_pr"
			issue.RunError = tc.captured
			issue.ExitCode = app.ExitCodex
			var id string
			if tc.legacy {
				id = saveLegacyIssueRepair(t, &q, issue).RequestID
			} else {
				var err error
				id, err = q.capture(context.Background(), issue)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.changedLogRoot {
				q.logRoot = t.TempDir()
			}
			if tc.current != "" || tc.incident != "" || tc.missingReceipt {
				if !tc.missingReceipt {
					issue.RepairRequestID = id
				}
				if tc.current != "" {
					issue.RunError = tc.current
				}
				if tc.incident != "" {
					issue.Incident = tc.incident
				}
				issue.Error = "temporary label removal failure"
				if err := claims.Save(issue); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				q.reconcile(context.Background(), func(context.Context, config.Config, bool, string, bool) (string, error) {
					t.Error("blocked dependency repair dispatched")
					return "", errors.New("unexpected dispatch")
				})
			}
			repairs, err := q.store.List()
			if err != nil || len(repairs) != 1 {
				t.Fatalf("repair records = %d, err = %v", len(repairs), err)
			}
			if got := repairs[0]; got.State != "blocked" || got.Started || got.Error != tc.want {
				t.Fatalf("repair state = %q, started = %t, error = %q", got.State, got.Started, got.Error)
			}
		})
	}
}

func saveLegacyIssueRepair(t *testing.T, q *issueRepairQueue, issue localruntime.IssueClaimRecord) localruntime.IssueRepairRecord {
	t.Helper()
	id := issueRepairID(issue)
	result := app.Result{ExitCode: issue.ExitCode, Err: errors.New(issue.FailureDetail())}
	record := localruntime.IssueRepairRecord{
		ID: id, RequestID: "repair-" + id, State: "queued",
		Run: failureFollowUpRunConfig(issue.RequestID, result, issue.Run, q.logRoot),
	}
	if err := q.store.Save(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestIssueRepairOutboxKeepsSupersededRolloutSkipped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		attempt, number    int
		state              string
		legacy, skip       bool
		sourceFulfilled    bool
		admissionRetry     bool
		waitForSlot        bool
		sourceReadError    bool
		unrelatedReadError bool
		ordinary           bool
	}{
		{name: "legacy source log", attempt: 3, number: 17, state: "fulfilled", legacy: true, skip: true},
		{name: "explicit source claim", attempt: 3, number: 17, state: "fulfilled", skip: true},
		{name: "source claim fulfilled", attempt: 3, number: 17, state: "incident", sourceFulfilled: true, skip: true},
		{name: "newer attempt fulfilled after enqueue rejection", attempt: 3, number: 17, state: "fulfilled", admissionRetry: true, skip: true},
		{name: "source fulfilled after enqueue rejection", attempt: 3, number: 17, state: "incident", sourceFulfilled: true, admissionRetry: true, skip: true},
		{name: "newer attempt fulfilled while waiting for a slot", attempt: 3, number: 17, state: "fulfilled", waitForSlot: true, skip: true},
		{name: "legacy source fulfilled while waiting for a slot", attempt: 3, number: 17, state: "incident", legacy: true, sourceFulfilled: true, waitForSlot: true, skip: true},
		{name: "unfulfilled retry while waiting for a slot", attempt: 3, number: 17, state: "incident", waitForSlot: true},
		{name: "source read failed after scheduler admission", attempt: 3, number: 17, state: "incident", waitForSlot: true, sourceReadError: true},
		{name: "unrelated malformed claim after admission", attempt: 3, number: 18, state: "incident", waitForSlot: true, unrelatedReadError: true},
		{name: "legacy source with unrelated malformed claim", attempt: 3, number: 18, state: "incident", waitForSlot: true, legacy: true, unrelatedReadError: true},
		{name: "ordinary repair with newer fulfillment", attempt: 3, number: 17, state: "fulfilled", ordinary: true},
		{name: "ordinary repair with fulfilled source", attempt: 3, number: 17, state: "incident", sourceFulfilled: true, ordinary: true},
		{name: "different issue", attempt: 3, number: 18, state: "fulfilled"},
		{name: "older fulfillment", attempt: 1, number: 17, state: "fulfilled"},
		{name: "unfulfilled retry", attempt: 3, number: 17, state: "incident"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, source := lifecycleRecord(t)
			q := issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
			source.RunError = "codex: codex reported failure: Failure: Production rollout remains pending. Error details: IAM setup and Secrets Manager configuration are outstanding."
			source.Incident = localruntime.IssueIncidentExternalRollout
			source.Outcome = "error"
			source.Run.GitHubIssue.Claim.Attempt = 2
			if tc.ordinary {
				source.RunError = "internal invariant failed"
				source.Incident = "missing_pr"
			}
			record := saveLegacyIssueRepair(t, &q, source)
			if !tc.ordinary {
				record.State = "blocked"
				record.Error = failurefollowup.ExternalRolloutPrerequisitesReason
			}
			if !tc.legacy {
				record.SourceClaimID = source.ID
			}
			source.RepairRequestID = record.RequestID
			if err := q.store.Save(record); err != nil {
				t.Fatal(err)
			}
			other := source
			other.ID = strings.Repeat("c", 32)
			other.RequestID = "issue-" + other.ID
			other.State = tc.state
			other.RepairRequestID = ""
			cfg := *source.Run.GitHubIssue
			cfg.Number = tc.number
			claim := *cfg.Claim
			claim.ID = other.ID
			claim.Attempt = tc.attempt
			cfg.Claim = &claim
			other.Run.GitHubIssue = &cfg
			if other.State == "fulfilled" {
				other.VerifiedAt = time.Now()
			}
			calls := 0
			enqueue := func(context.Context, config.Config, bool, string, bool) (string, error) {
				calls++
				return "", errors.New("admission unavailable")
			}
			if tc.admissionRetry {
				if err := claims.Save(source); err != nil {
					t.Fatal(err)
				}
				q.reconcile(t.Context(), enqueue)
				repairs, err := q.store.List()
				if err != nil || len(repairs) != 1 || calls != 1 || repairs[0].State != "queued" || repairs[0].NextAttemptAt.IsZero() {
					t.Fatalf("reopened repair was not queued for retry: calls=%d repairs=%+v err=%v", calls, repairs, err)
				}
				record = repairs[0]
				record.NextAttemptAt = time.Time{}
				if err := q.store.Save(record); err != nil {
					t.Fatal(err)
				}
				calls = 0
			}
			var waiting *issueRepairWork
			if tc.waitForSlot {
				if err := claims.Save(source); err != nil {
					t.Fatal(err)
				}
				q.reconcile(t.Context(), func(ctx context.Context, _ config.Config, _ bool, _ string, _ bool) (string, error) {
					waiting = ctx.Value(issueRepairContextKey{}).(*issueRepairWork)
					return waiting.record.RequestID, nil
				})
				if waiting == nil {
					t.Fatal("reopened repair was not admitted to the queue")
				}
				t.Cleanup(func() { waiting.lock.Close() })
			}
			if tc.sourceFulfilled {
				source.State = "fulfilled"
				source.VerifiedAt = time.Now()
			}
			if err := claims.Save(source); err != nil {
				t.Fatal(err)
			}
			if err := claims.Save(other); err != nil {
				t.Fatal(err)
			}
			if waiting != nil {
				if tc.sourceReadError {
					if err := os.WriteFile(filepath.Join(claims.Root, source.ID+".json"), []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if tc.unrelatedReadError {
					if err := os.WriteFile(filepath.Join(claims.Root, other.ID+".json"), []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				err := waiting.start()
				waiting.finish("", app.Result{Err: err})
				repairs, readErr := q.store.List()
				if readErr != nil || len(repairs) != 1 {
					t.Fatalf("repair records = %d, err = %v", len(repairs), readErr)
				}
				got := repairs[0]
				switch {
				case tc.skip:
					if !errors.Is(err, errIssueRepairSourceFulfilled) || got.Started || got.State != "blocked" || got.Error != issueRepairSourceFulfilledReason {
						t.Fatalf("superseded repair: started=%t state=%q error=%q, start error=%v", got.Started, got.State, got.Error, err)
					}
				case tc.sourceReadError:
					if !errors.Is(err, errIssueRepairSourceCheck) || got.Started || got.State != "queued" || got.NextAttemptAt.IsZero() {
						t.Fatalf("source check error did not defer repair: started=%t state=%q error=%v", got.Started, got.State, err)
					}
				default:
					if err != nil || !got.Started {
						t.Fatalf("eligible repair failed to start: started=%t error=%v", got.Started, err)
					}
				}
				return
			}
			for range 2 {
				q.reconcile(t.Context(), enqueue)
			}
			repairs, err := q.store.List()
			if err != nil || len(repairs) != 1 {
				t.Fatalf("repair records = %d, err = %v", len(repairs), err)
			}
			if tc.skip {
				if calls != 0 || repairs[0].State != "blocked" || repairs[0].Error != issueRepairSourceFulfilledReason {
					t.Fatalf("superseded repair changed or dispatched: calls=%d state=%q error=%q", calls, repairs[0].State, repairs[0].Error)
				}
			} else if calls != 1 || repairs[0].State != "queued" {
				t.Fatalf("repair should remain eligible: calls=%d state=%q", calls, repairs[0].State)
			}
		})
	}
}

func TestIssueRepairDependencyBlockDoesNotSuppressAnotherClaim(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		name := "new claim"
		if legacy {
			name = "legacy shared receipt"
		}
		t.Run(name, func(t *testing.T) {
			testIssueRepairDependencyBlockDoesNotSuppressAnotherClaim(t, legacy)
		})
	}
}

func testIssueRepairDependencyBlockDoesNotSuppressAnotherClaim(t *testing.T, legacy bool) {
	t.Helper()
	claims, blocked := lifecycleRecord(t)
	q := issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
	blocked.Incident = "missing_pr"
	blocked.RunError = "worker failed in /private/first"
	other := blocked
	other.ID = strings.Repeat("b", 32)
	other.RequestID = "issue-" + other.ID
	other.RunError = "worker failed in /private/second"
	otherIssue := *blocked.Run.GitHubIssue
	otherClaim := *otherIssue.Claim
	otherClaim.ID = other.ID
	otherIssue.Claim = &otherClaim
	otherIssue.Number++
	other.Run.GitHubIssue = &otherIssue
	var blockedID string
	if legacy {
		blockedID = saveLegacyIssueRepair(t, &q, blocked).RequestID
		q.logRoot = t.TempDir()
		if id, err := q.capture(context.Background(), blocked); err != nil || id != blockedID {
			t.Fatalf("source claim capture = %q, %v, want %q", id, err, blockedID)
		}
		blocked.RepairRequestID = blockedID
		other.RepairRequestID = blockedID
		if err := claims.Save(other); err != nil {
			t.Fatal(err)
		}
	} else {
		var err error
		blockedID, err = q.capture(context.Background(), blocked)
		if err != nil {
			t.Fatal(err)
		}
		if shared, err := q.capture(context.Background(), other); err != nil || shared != blockedID {
			t.Fatalf("matching actionable failure captured as %q, %v, want %q", shared, err, blockedID)
		}
	}
	blocked.Incident = localruntime.IssueIncidentOpenDependency
	if err := claims.Save(blocked); err != nil {
		t.Fatal(err)
	}
	q.reconcile(context.Background(), func(context.Context, config.Config, bool, string, bool) (string, error) {
		t.Fatal("blocked dependency repair dispatched")
		return "", nil
	})
	if legacy {
		stored, err := claims.Load(other.ID)
		if err != nil || stored.RepairRequestID != "" {
			t.Fatalf("unrelated claim still points at blocked repair: %q, %v", stored.RepairRequestID, err)
		}
		owner, err := claims.Load(blocked.ID)
		if err != nil || owner.RepairRequestID != blockedID {
			t.Fatalf("source receipt = %q, %v, want %q", owner.RepairRequestID, err, blockedID)
		}
		other = stored
	} else if err := claims.Save(other); err != nil {
		t.Fatal(err)
	}
	otherID, err := q.capture(context.Background(), other)
	if err != nil || otherID == blockedID {
		t.Fatalf("unrelated claim captured as %q, err = %v, blocker = %q", otherID, err, blockedID)
	}
	if again, err := q.capture(context.Background(), other); err != nil || again != otherID {
		t.Fatalf("unrelated claim recaptured as %q, %v, want %q", again, err, otherID)
	}
	var dispatched []string
	q.reconcile(context.Background(), func(ctx context.Context, _ config.Config, _ bool, _ string, _ bool) (string, error) {
		work := ctx.Value(issueRepairContextKey{}).(*issueRepairWork)
		dispatched = append(dispatched, work.record.RequestID)
		return work.record.RequestID, work.lock.Close()
	})
	if len(dispatched) != 1 || dispatched[0] != otherID {
		t.Fatalf("dispatched %v, want only %q", dispatched, otherID)
	}
}

func TestIssueRepairOutboxWithCorruptClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		corruptSource bool
		wantCalls     int
	}{
		{name: "unrelated claim", wantCalls: 1},
		{name: "source claim", corruptSource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			q := issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logRoot: t.TempDir(), logf: t.Logf}
			issue.RunError = "internal invariant failed"
			id, err := q.capture(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			issue.RepairRequestID = id
			if err := claims.Save(issue); err != nil {
				t.Fatal(err)
			}
			corruptID := strings.Repeat("b", 32)
			if tc.corruptSource {
				corruptID = issue.ID
				other := issue
				other.ID = strings.Repeat("b", 32)
				other.RequestID = "issue-" + other.ID
				other.RepairRequestID = ""
				otherIssue := *issue.Run.GitHubIssue
				otherClaim := *otherIssue.Claim
				otherClaim.ID = other.ID
				otherIssue.Claim = &otherClaim
				other.Run.GitHubIssue = &otherIssue
				if err := claims.Save(other); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(claims.Root, corruptID+".json"), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			q.reconcile(context.Background(), func(ctx context.Context, _ config.Config, _ bool, _ string, _ bool) (string, error) {
				calls++
				work := ctx.Value(issueRepairContextKey{}).(*issueRepairWork)
				return work.record.RequestID, work.lock.Close()
			})
			if calls != tc.wantCalls {
				t.Fatalf("dispatches = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
