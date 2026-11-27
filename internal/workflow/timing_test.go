package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/tasktiming"
)

func TestWorkflowTimingPersistsCheckWaitThroughRestartAndMerge(t *testing.T) {
	t.Parallel()
	w := definition("delivery")
	w.IncludeBacklog = true
	w.ReviewCycles = 1
	w.Merge.Enabled = true
	w.Merge.RequireApproval = false
	e, items, _ := testEngine(t, w)
	*items = []Item{{Repository: w.Repository, Kind: "issue", Number: 7, Open: true, Labels: []string{"ready"}}}
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	now := start
	e.Now = func() time.Time { return now }
	var timing, aggregate strings.Builder
	e.TimingLog = func(format string, args ...any) { fmt.Fprintf(&timing, format+"\n", args...) }
	e.Logf = func(format string, args ...any) { fmt.Fprintf(&aggregate, format+"\n", args...) }
	checksPassed := false

	e.Merge = func(context.Context, Run) error {
		now = now.Add(3 * time.Second)
		return nil
	}

	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	now = start.Add(10 * time.Second)
	prURL := "https://github.com/acme/repo/pull/7"
	if err := e.Complete(runs[0].ID, Outcome{
		Status: "succeeded", PRURL: prURL, TimingAttempt: "worker-attempt", ReviewReceipt: fixtureRunReviewReceipt(runs[0], prURL),
	}); err != nil {
		t.Fatal(err)
	}
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		return Item{Open: true, ChecksPassed: checksPassed, HeadSHA: fixtureRunReviewReceipt(runs[0], prURL).Head}, nil
	}
	tick(t, e)

	// A fresh engine has only the saved workflow state and the same clock.
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed,
		Now: e.Now, Fetch: e.Fetch, Status: e.Status, InspectPR: e.InspectPR, Merge: e.Merge, TimingLog: e.TimingLog, Logf: e.Logf}
	now = start.Add(30 * time.Second)
	tick(t, restarted)
	now = start.Add(60 * time.Second)
	checksPassed = true
	tick(t, restarted)

	spans, err := tasktiming.Read(t.Context(), strings.NewReader(timing.String()), "task")
	if err != nil || len(spans) != 3 {
		t.Fatalf("timeline = %+v, err = %v\n%s\n%s", spans, err, timing.String(), aggregate.String())
	}
	for i, want := range []struct {
		stage tasktiming.Stage
		ms    int64
	}{{tasktiming.Checks, 50000}, {tasktiming.Merge, 3000}, {tasktiming.WorkflowTotal, 63000}} {
		span := spans[i]
		if span.Stage != want.stage || span.WallMS == nil || *span.WallMS != want.ms || span.Attempt != "worker-attempt" || span.AgentMS != nil {
			t.Fatalf("timing mismatch for %s:\n%s", want.stage, timing.String())
		}
	}
	if !strings.Contains(aggregate.String(), "status=merged reason=merged duration_ms=63000") {
		t.Fatalf("aggregate timing mismatch:\n%s", aggregate.String())
	}
	for _, private := range []string{"acme/repo", "fixture-head", prURL, w.Task.Instructions} {
		if private != "" && strings.Contains(timing.String(), private) {
			t.Fatalf("timing leaked task data:\n%s", timing.String())
		}
	}
}

func TestFailedWorkflowTimingUsesCompletionAndKeepsUnknownStart(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, restoreWithoutStart := range []bool{false, true} {
		run := Run{ID: "activation", RequestID: "task", Status: "failed", UpdatedAt: start.Add(time.Hour),
			StartedAt: &start, Outcome: &Outcome{CompletedAt: start.Add(4 * time.Second)}}
		if restoreWithoutStart {
			run.StartedAt = nil
		}
		var log strings.Builder
		engine := Engine{TimingLog: func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }}
		engine.logRun(run, "transition", time.Time{})
		totals, err := tasktiming.Read(t.Context(), strings.NewReader(log.String()), "task")
		if err != nil || len(totals) != 1 || !run.Outcome.CompletedAt.Equal(*totals[0].EndedAt) {
			t.Fatalf("failure timing = %+v, err = %v\n%s", totals, err, log.String())
		}
		if restoreWithoutStart {
			if totals[0].StartedAt != nil || totals[0].WallMS != nil {
				t.Fatalf("missing start was replaced with inferred values:\n%s", log.String())
			}
		} else if totals[0].WallMS == nil || *totals[0].WallMS != 4000 {
			t.Fatalf("failed duration did not use completion:\n%s", log.String())
		}
	}
}

func TestSameTaskRetryKeepsTimingAndCapturedPolicy(t *testing.T) {
	t.Parallel()
	w := definition("delivery")
	w.Merge.Enabled = true
	w.ReviewCycles = 1
	e, _, _ := testEngine(t, w)
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	now := start.Add(10 * time.Second)
	e.Now = func() time.Time { return now }
	var log strings.Builder
	e.TimingLog = func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }
	item := Item{Repository: w.Repository, Kind: "issue", Number: 7}
	wait := tasktiming.Start("first-attempt", "wait", tasktiming.WorkflowWait, start.Add(3*time.Second))
	wait.Write(e.TimingLog, "task")
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	captured := ExecutionPolicy{ReviewCycles: 1}
	state.Runs = []Run{{ID: "first", RequestID: "task", Status: awaitingReview, Workflow: w, Item: item,
		Execution: &captured, StartedAt: &start, TimingWait: &wait}}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}
	id, err := e.DispatchRetry(t.Context(), w.ID, w.ExecutionPolicyHash(), "first", item, &captured, func(retry Run) (string, error) {
		if retry.ID == "" || retry.ReviewPolicy().ReviewCycles != captured.ReviewCycles || !retry.StartedAt.Equal(start) {
			t.Fatalf("retry lost original duration boundary or captured execution policy: %+v", retry)
		}
		return "task", nil
	})
	if err != nil || id != "task" {
		t.Fatalf("retry = %q, %v", id, err)
	}
	spans, err := tasktiming.Read(t.Context(), strings.NewReader(log.String()), id)
	if err != nil || len(spans) != 1 || spans[0].Attempt != "first-attempt" || spans[0].WallMS == nil || *spans[0].WallMS != 7000 {
		t.Fatalf("prior attempt history = %+v, err = %v\n%s", spans, err, log.String())
	}
}
