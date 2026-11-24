package workflow

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
			if scheduled {
				w.IncludeBacklog = false
				now := time.Now().UTC().Add(-time.Hour)
				w.Trigger = Trigger{Event: "schedule", IntervalHours: 24, StartAt: &now}
			}
			e, items, _ := testEngine(t, w)
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
			if err = e.Complete(runs[0].ID, Outcome{Status: "failed", Reason: "Private instructions https://github.com/acme/repo /private/workspace"}); err != nil {
				t.Fatal(err)
			}
			tick(t, e)
			tick(t, e)
			if len(logs) != 3 || !strings.Contains(logs[2], "event=transition") || !strings.Contains(logs[2], "status=failed") {
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
