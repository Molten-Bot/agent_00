package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

// ---- Run: guard clauses and default wiring ---------------------------------

func TestRunNilGuardsAndDefaultPollInterval(t *testing.T) {
	t.Parallel()

	var nilMonitor *PRMergeMonitor
	if err := nilMonitor.Run(context.Background()); err != nil {
		t.Fatalf("nil monitor Run() error = %v, want nil", err)
	}
	if err := (&PRMergeMonitor{}).Run(context.Background()); err != nil {
		t.Fatalf("Run(no broker, no runner) error = %v, want nil", err)
	}
	if err := (&PRMergeMonitor{Broker: NewBroker()}).Run(context.Background()); err != nil {
		t.Fatalf("Run(no runner) error = %v, want nil", err)
	}

	monitor := &PRMergeMonitor{
		Broker: NewBroker(),
		Runner: &stubPRMonitorRunner{result: execx.Result{Stdout: `{}`}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Run so the loop exits right after the first pollOnce
	if err := monitor.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if monitor.PollInterval != defaultPRMergePollInterval {
		t.Fatalf("PollInterval after Run() = %s, want default %s", monitor.PollInterval, defaultPRMergePollInterval)
	}
}

// ---- shouldMonitorTaskPR / beginCheck / markMerged / forgetMissingTasks ----

func TestShouldMonitorTaskPR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		task Task
		want bool
	}{
		{"no pr url", Task{Status: "completed", PRURL: ""}, false},
		{"completed with pr url", Task{Status: "completed", PRURL: "url"}, true},
		{"no_changes with pr url", Task{Status: "no_changes", PRURL: "url"}, true},
		{"running with pr url", Task{Status: "running", PRURL: "url"}, false},
		{"errored with pr url", Task{Status: "error", PRURL: "url"}, false},
	}
	for _, tt := range tests {
		if got := shouldMonitorTaskPR(tt.task); got != tt.want {
			t.Fatalf("%s: shouldMonitorTaskPR() = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestBeginCheckRejectsBlankInFlightAndMergedRequests(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{}
	if m.beginCheck("") {
		t.Fatal("beginCheck(blank) = true, want false")
	}
	if !m.beginCheck("req-1") {
		t.Fatal("beginCheck(first) = false, want true")
	}
	if m.beginCheck("req-1") {
		t.Fatal("beginCheck(already in flight) = true, want false")
	}
	m.endCheck("req-1")
	if !m.beginCheck("req-1") {
		t.Fatal("beginCheck(after endCheck) = false, want true")
	}
	m.endCheck("req-1")
	m.markMerged("req-1")
	if m.beginCheck("req-1") {
		t.Fatal("beginCheck(merged) = true, want false")
	}
}

func TestMarkMergedIgnoresBlankRequestID(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{}
	m.markMerged("")
	if len(m.merged) != 0 {
		t.Fatalf("len(merged) after blank markMerged = %d, want 0", len(m.merged))
	}
	m.markMerged("req-1")
	if _, ok := m.merged["req-1"]; !ok {
		t.Fatal("merged[req-1] missing after markMerged")
	}
}

func TestForgetMissingTasksPrunesStaleEntries(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{
		merged: map[string]struct{}{"req-old": {}, "req-keep": {}},
	}
	m.forgetMissingTasks(map[string]struct{}{"req-keep": {}})

	if _, ok := m.merged["req-old"]; ok {
		t.Fatal("merged[req-old] still present, want pruned")
	}
	if _, ok := m.merged["req-keep"]; !ok {
		t.Fatal("merged[req-keep] missing, want retained")
	}
}

// ---- checkTaskPR: MarkTaskPRMerged error paths -----------------------------

func TestCheckTaskPRMarksMergedWhenTaskNotFoundInBroker(t *testing.T) {
	t.Parallel()

	broker := NewBroker() // no task recorded for req-missing
	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z"}`}}
	monitor := &PRMergeMonitor{Runner: runner, Broker: broker, Logf: func(string, ...any) {}}

	monitor.checkTaskPR(context.Background(), Task{RequestID: "req-missing", PRURL: "https://github.com/acme/repo/pull/5"})

	monitor.mu.Lock()
	_, ok := monitor.merged["req-missing"]
	monitor.mu.Unlock()
	if !ok {
		t.Fatal("merged[req-missing] missing, want set after ErrTaskNotFound from MarkTaskPRMerged")
	}
}

func TestCheckTaskPRLogsWarnAndSkipsMarkMergedWhenTaskNotCompleted(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.IngestLog("dispatch status=start request_id=req-running repo=acme/repo")

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z"}`}}
	logs := make(chan string, 4)
	monitor := &PRMergeMonitor{
		Runner: runner,
		Broker: broker,
		Logf: func(format string, args ...any) {
			select {
			case logs <- fmt.Sprintf(format, args...):
			default:
			}
		},
	}

	task, ok := broker.Task("req-running")
	if !ok {
		t.Fatal("broker.Task(req-running) found = false")
	}
	task.PRURL = "https://github.com/acme/repo/pull/6"

	monitor.checkTaskPR(context.Background(), task)

	monitor.mu.Lock()
	_, merged := monitor.merged["req-running"]
	monitor.mu.Unlock()
	if merged {
		t.Fatal("merged[req-running] present, want absent (ErrTaskNotCompleted must not call markMerged)")
	}

	select {
	case line := <-logs:
		if !strings.Contains(line, "pr_monitor_mark_merged") {
			t.Fatalf("log line = %q, want pr_monitor_mark_merged warning", line)
		}
	default:
		t.Fatal("expected a warning log line for the ErrTaskNotCompleted path")
	}
}

func TestCheckTaskPRLogsWarnWhenCleanupTaskFails(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.RecordTaskRunConfig("req-cleanup-fails", []byte(`{"repo":"acme/repo","prompt":"ship it"}`))
	broker.IngestLog("dispatch status=start request_id=req-cleanup-fails repo=acme/repo")
	broker.IngestLog("dispatch status=completed request_id=req-cleanup-fails workspace=/tmp/run branch=agent00-ship pr_url=https://github.com/acme/repo/pull/44")

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z"}`}}
	logs := make(chan string, 4)
	monitor := &PRMergeMonitor{
		Runner: runner,
		Broker: broker,
		Logf: func(format string, args ...any) {
			select {
			case logs <- fmt.Sprintf(format, args...):
			default:
			}
		},
		CleanupTask: func(context.Context, string) error {
			return errors.New("cleanup failed")
		},
	}

	task, ok := broker.Task("req-cleanup-fails")
	if !ok {
		t.Fatal("broker.Task() found = false")
	}
	monitor.checkTaskPR(context.Background(), task)

	monitor.mu.Lock()
	_, merged := monitor.merged["req-cleanup-fails"]
	monitor.mu.Unlock()
	if !merged {
		t.Fatal("merged[req-cleanup-fails] missing, want set even when cleanup fails")
	}

	select {
	case line := <-logs:
		if !strings.Contains(line, "pr_monitor_cleanup") {
			t.Fatalf("log line = %q, want pr_monitor_cleanup warning", line)
		}
	default:
		t.Fatal("expected a warning log line for the failed cleanup")
	}
}

func TestPRStateRequiresNonBlankURL(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{Runner: &stubPRMonitorRunner{}}
	_, err := m.prState(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "pull request url is required") {
		t.Fatalf("prState(blank url) error = %v, want pull request url is required", err)
	}
}

func TestPRStateReturnsDecodeErrorForMalformedJSON(t *testing.T) {
	t.Parallel()

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: "not-json"}}
	m := &PRMergeMonitor{Runner: runner}
	_, err := m.prState(context.Background(), "https://github.com/acme/repo/pull/1")
	if err == nil || !strings.Contains(err.Error(), "decode gh pr view response") {
		t.Fatalf("prState(malformed json) error = %v, want decode gh pr view response", err)
	}
}
