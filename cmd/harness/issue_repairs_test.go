package main

import (
	"context"
	"errors"
	"fmt"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, issue := lifecycleRecord(t)
			q := issueRepairQueue{store: &localruntime.IssueRepairStore{Root: t.TempDir()}, claims: claims, logf: t.Logf}
			issue.Incident = "missing_pr"
			issue.RunError = tc.captured
			issue.ExitCode = app.ExitCodex
			id, err := q.capture(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if tc.current != "" || tc.incident != "" {
				issue.RepairRequestID = id
				issue.RunError = tc.current
				issue.Incident = tc.incident
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
