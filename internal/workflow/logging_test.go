package workflow

import (
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
				for _, field := range []string{"workflow_id=", "run_id=", "request_id=", "status=", "reason="} {
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
		elapsed         time.Duration
		want            string
	}{
		{name: "success", outcome: Outcome{Status: "succeeded"}, elapsed: 3 * time.Second, want: "status=succeeded reason=succeeded duration_ms=3000"},
		{name: "zero elapsed", outcome: Outcome{Status: "succeeded"}, want: "status=succeeded reason=succeeded duration_ms=0"},
		{name: "review interrupted", outcome: Outcome{Status: "failed", ReviewInterrupted: true}, elapsed: 3 * time.Second, want: "status=failed reason=external_merge duration_ms=3000"},
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

			s, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			s.Runs = []Run{{ID: "activation", Workflow: w, Item: Item{Repository: w.Repository}, Status: "running",
				StartedAt: &start, UpdatedAt: start, Outcome: &test.outcome}}
			if err := e.save(s); err != nil {
				t.Fatal(err)
			}

			tick(t, e)
			if len(logs) != 1 || !strings.Contains(logs[0], "event=transition") || !strings.HasSuffix(logs[0], test.want) {
				t.Fatalf("terminal logs = %v, want one transition ending in %q", logs, test.want)
			}
		})
	}
}
