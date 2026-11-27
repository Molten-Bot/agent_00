package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

func TestWorkflowLifecycleLogsAreBoundedAndDoNotRepeat(t *testing.T) {
	t.Parallel()
	for _, scheduled := range []bool{false, true} {
		t.Run(fmt.Sprint(scheduled), func(t *testing.T) {
			t.Parallel()
			w := definition("diagnostics")
			w.Name = "Private task name"
			w.Task.Instructions = "Private instructions"
			w.IncludeBacklog = true
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if scheduled {
				w.IncludeBacklog = false
				due := now.Add(-time.Hour)
				w.Trigger = Trigger{Event: "schedule", IntervalHours: 24, StartAt: &due}
			}
			e, items, _ := testEngine(t, w)
			e.Now = func() time.Time { return now }
			*items = []Item{{Repository: w.Repository, Number: 1, Kind: "issue", Open: true, Labels: []string{"ready"}}}
			var logs []string
			e.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			tick(t, e)
			tick(t, e)
			if len(logs) != 2 || !strings.Contains(logs[0], "event=dispatch") || !strings.Contains(logs[1], "event=submission") {
				t.Fatalf("dispatch logs: %v", logs)
			}
			runs, err := e.Runs()
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(3 * time.Second)
			if err = e.Complete(runs[0].ID, Outcome{Status: "failed", Reason: "Private instructions https://github.com/acme/repo /private/workspace"}); err != nil {
				t.Fatal(err)
			}
			tick(t, e)
			tick(t, e)
			if len(logs) != 3 || !strings.Contains(logs[2], "event=transition") || !strings.HasSuffix(logs[2], "status=failed reason=failed duration_ms=3000") {
				t.Fatalf("completion logs: %v", logs)
			}
			for _, line := range logs {
				if strings.Contains(line, "Private") || strings.Contains(line, "acme/repo") || strings.Contains(line, "/private/") {
					t.Fatalf("private context leaked: %s", line)
				}
				for _, field := range []string{"workflow_id=", "run_id=", "request_id=", "started_at=" + now.Add(-3*time.Second).Format(time.RFC3339Nano), "status=", "reason="} {
					if !strings.Contains(line, field) {
						t.Fatalf("missing %s: %s", field, line)
					}
				}
			}
		})
	}
}

func TestWorkflowTerminalTransitionDuration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		outcome         Outcome
		workflowRemoved bool
		alreadyMerged   bool
		hasReceipt      bool
		elapsed         time.Duration
		want            string
	}{
		{name: "success", outcome: Outcome{Status: "succeeded"}, elapsed: 3 * time.Second, want: "status=succeeded reason=succeeded duration_ms=3000"},
		{name: "zero elapsed", outcome: Outcome{Status: "succeeded"}, want: "status=succeeded reason=succeeded duration_ms=0"},
		{name: "archived review interrupted", outcome: Outcome{Status: "failed", ReviewInterrupted: true}, alreadyMerged: true, elapsed: time.Hour, want: "status=merged reason=external_merge duration_ms=3000"},
		{name: "merged before review writeback", outcome: Outcome{Status: "succeeded", PendingReviewGate: &githubutil.PendingReviewGate{}}, alreadyMerged: true, hasReceipt: true, elapsed: 3 * time.Second, want: "status=merged reason=merge_unconfirmed duration_ms=3000"},
		{name: "merged before review writeback without receipt", outcome: Outcome{Status: "succeeded", PendingReviewGate: &githubutil.PendingReviewGate{}}, alreadyMerged: true, elapsed: 3 * time.Second, want: "status=merged reason=merge_unconfirmed duration_ms=3000"},
		{name: "completed without merge", outcome: Outcome{Status: "succeeded", PendingReviewGate: &githubutil.PendingReviewGate{}}, workflowRemoved: true, elapsed: 3 * time.Second, want: "status=completed reason=completed duration_ms=3000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			w := definition("delivery")
			workflows := []Definition{w}
			if test.workflowRemoved {
				workflows = nil
			}
			e, _, _ := testEngine(t, workflows...)
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			e.Now = func() time.Time { return start.Add(test.elapsed) }
			var logs []string
			e.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			openWithoutReceipt := test.alreadyMerged && !test.hasReceipt && !test.outcome.ReviewInterrupted
			if test.alreadyMerged {
				const prURL = "https://github.com/acme/repo/pull/42"
				test.outcome.PRURL = prURL
				if test.hasReceipt {
					test.outcome.ReviewReceipt = fixtureReviewReceipt(w, prURL)
				}
				inspect := func(_ context.Context, repo, url string) (Item, error) {
					if repo != w.Repository || url != prURL {
						t.Fatalf("inspected %s %s", repo, url)
					}
					if openWithoutReceipt {
						return Item{Open: true}, nil
					}
					return Item{Merged: true, MergedAt: start.Add(3 * time.Second)}, nil
				}
				e.InspectPRState = inspect
				e.InspectPR = func(ctx context.Context, repo, url string) (Item, error) {
					if !test.hasReceipt {
						t.Fatal("ran full inspection without a usable review receipt")
					}
					return inspect(ctx, repo, url)
				}
				e.FinishReview = func(context.Context, githubutil.ReviewReceipt, *githubutil.PendingReviewGate) error {
					t.Fatal("review writeback ran after merge")
					return nil
				}
			}

			s, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			s.Runs = []Run{{ID: "activation", Workflow: w, Item: Item{Repository: w.Repository}, Status: "running",
				StartedAt: &start, UpdatedAt: start, Outcome: &test.outcome}}
			if err := e.save(s); err != nil {
				t.Fatal(err)
			}

			if openWithoutReceipt {
				tick(t, e)
				tick(t, e)
				if len(logs) != 1 || !strings.Contains(logs[0], "status=awaiting_review reason=review_not_clean") {
					t.Fatalf("open pull request without receipt did not wait for review: %v", logs)
				}
				openWithoutReceipt = false
				logs = nil
			}
			tick(t, e)
			if len(logs) != 1 || !strings.Contains(logs[0], "event=transition") || !strings.HasSuffix(logs[0], test.want) {
				t.Fatalf("terminal logs = %v, want one transition ending in %q", logs, test.want)
			}
			if test.alreadyMerged && !strings.Contains(logs[0], "merged_at="+start.Add(3*time.Second).Format(time.RFC3339Nano)) {
				t.Fatalf("merge log omitted GitHub timestamp: %s", logs[0])
			}
		})
	}
}

func TestWorkflowRetryDurationIncludesRetiredActivationAfterRestart(t *testing.T) {
	t.Parallel()
	for _, recovery := range []string{"dispatch", "issue recovery", "same event issue recovery"} {
		t.Run(recovery, func(t *testing.T) {
			t.Parallel()
			w := definition("retry-timing")
			w.Merge.Enabled = true
			e, _, _ := testEngine(t, w)
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := start.Add(time.Minute)
			e.Now = func() time.Time { return now }
			item := Item{Repository: w.Repository, Kind: "issue", Number: 1}
			if err := e.transaction(func(s *State) error {
				s.Runs = []Run{{ID: "original", RequestID: "original-task", Workflow: w, Item: item,
					Status: "running", StartedAt: &start, Outcome: &Outcome{Status: "failed"}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			tick(t, e)
			if runs, err := e.Runs(); err != nil || len(runs) != 0 {
				t.Fatalf("failed activation was not retired: %+v, %v", runs, err)
			}

			now = now.Add(20 * time.Second)
			var logs []string
			restarted := &Engine{
				Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Now: e.Now,
				Fetch: e.Fetch, Status: e.Status, Reconcile: func(string) string { return "retry-task" },
				InspectPR: func(context.Context, string, string) (Item, error) { return Item{Merged: true}, nil },
				Logf:      func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
			}
			if recovery == "dispatch" {
				if _, err := restarted.DispatchRetry(t.Context(), w.ID, w.ExecutionPolicyHash(), "original", item, nil, func(Run) (string, error) {
					return "retry-task", nil
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				event, retryOf := "retry", "original"
				if recovery == "same event issue recovery" {
					event, retryOf = "original", ""
				}
				if err := restarted.PrepareIssueRecovery(w.ID, w.ExecutionPolicyHash(), event, retryOf, item, nil); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := restarted.Runs()
			if err != nil || len(runs) != 1 {
				t.Fatalf("retry activation = %+v, %v", runs, err)
			}
			now = now.Add(10 * time.Second)
			if err := restarted.Complete(runs[0].ID, Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/42"}); err != nil {
				t.Fatal(err)
			}
			tick(t, restarted)
			if len(logs) == 0 || !strings.HasSuffix(logs[len(logs)-1], "status=merged reason=merge_unconfirmed duration_ms=90000") ||
				!strings.Contains(logs[len(logs)-1], "started_at="+start.Format(time.RFC3339Nano)) {
				t.Fatalf("retry logs = %v, want original start and 90-second duration", logs)
			}
		})
	}
}
