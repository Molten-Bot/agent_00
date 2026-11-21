package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/app"
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
