package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestIssueRepairCaptureUsesWorkerFailureAfterUnlockError(t *testing.T) {
	t.Parallel()
	q := issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, logf: t.Logf}
	issue := localruntime.IssueClaimRecord{
		Step: "agent", Incident: "missing_pr", Outcome: "error", ExitCode: app.ExitCodex,
		Error: "temporary label removal failure", RunError: "worker failed in /private/workspace",
	}
	id, err := q.capture(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	issue.Error = "different unlock failure"
	if retryID, err := q.capture(context.Background(), issue); err != nil || retryID != id {
		t.Fatalf("same worker failure queued a different repair: %q, %v", retryID, err)
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
			id, err := q.capture(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				repairs, err := q.store.List()
				if err != nil || len(repairs) != 1 {
					t.Fatalf("repair records = %d, err = %v", len(repairs), err)
				}
				repairs[0].SourceClaimID = ""
				if err := q.store.Save(repairs[0]); err != nil {
					t.Fatal(err)
				}
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
