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
	if total := spans[2]; total.EndedAt == nil || !total.EndedAt.Equal(now) {
		t.Fatalf("workflow total did not end at coordinator merge completion:\n%s", timing.String())
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

func TestObserveIssueMergeClosesWaitBeforeRetiringActivation(t *testing.T) {
	t.Parallel()
	w := definition("delivery")
	e, _, _ := testEngine(t, w)
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	mergedAt := start.Add(10 * time.Second)
	e.Now = func() time.Time { return mergedAt.Add(time.Minute) }
	const prURL = "https://github.com/acme/repo/pull/7"
	wait := tasktiming.Start("worker-attempt", "wait", tasktiming.Checks, start)
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	state.Runs = []Run{{ID: "activation", RequestID: "task", Status: "waiting", Workflow: w,
		Item: Item{Repository: w.Repository, Kind: "issue", Number: 7}, PRURL: prURL, TimingWait: &wait}}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	var closed int
	e.TimingLog = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, " span=wait ") {
			closed++
			persisted, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			if r := persisted.run("activation"); r == nil || r.TimingWait == nil || r.TimingWait.ID != wait.ID {
				t.Fatal("activation lost its persisted wait before the completion was logged")
			}
		}
		fmt.Fprintln(&log, line)
	}
	for range 2 {
		if err := e.ObserveIssueMerge("activation", "task", prURL, mergedAt); err != nil {
			t.Fatal(err)
		}
	}
	if runs, err := e.Runs(); err != nil || len(runs) != 0 {
		t.Fatalf("runs after merge = %+v, err = %v", runs, err)
	}
	spans, err := tasktiming.Read(t.Context(), strings.NewReader(log.String()), "task")
	if err != nil || closed != 1 || len(spans) == 0 || spans[0].ID != wait.ID || spans[0].EndedAt == nil ||
		!spans[0].EndedAt.Equal(mergedAt) || spans[0].WallMS == nil || *spans[0].WallMS != 10000 {
		t.Fatalf("timing after observed merge = %+v, closed = %d, err = %v\n%s", spans, closed, err, log.String())
	}
}

func TestFailedWorkflowTimingUsesCompletionAndKeepsUnknownStart(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, restoreWithoutStart := range []bool{false, true} {
		w := definition("delivery")
		engine, _, _ := testEngine(t, w)
		engine.Now = func() time.Time { return start.Add(time.Hour) }
		run := Run{ID: "activation", RequestID: "task", Status: "running", Workflow: w,
			StartedAt: &start, Outcome: &Outcome{Status: "failed", CompletedAt: start.Add(4 * time.Second)}}
		if restoreWithoutStart {
			run.StartedAt = nil
		}
		var log strings.Builder
		engine.TimingLog = func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }
		state, err := engine.load()
		if err != nil {
			t.Fatal(err)
		}
		state.Runs = []Run{run}
		if err := engine.save(state); err != nil {
			t.Fatal(err)
		}
		tick(t, engine)
		totals, err := tasktiming.Read(t.Context(), strings.NewReader(log.String()), "task")
		if err != nil || len(totals) != 1 || totals[0].EndedAt == nil || !run.Outcome.CompletedAt.Equal(*totals[0].EndedAt) {
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

func TestWorkflowTotalIncludesWaitBeforePullRequestCloses(t *testing.T) {
	t.Parallel()
	w := definition("delivery")
	w.Merge.Enabled = true
	e, _, _ := testEngine(t, w)
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	now := start.Add(10 * time.Second)
	e.Now = func() time.Time { return now }
	var timing, aggregate strings.Builder
	e.TimingLog = func(format string, args ...any) { fmt.Fprintf(&timing, format+"\n", args...) }
	e.Logf = func(format string, args ...any) { fmt.Fprintf(&aggregate, format+"\n", args...) }
	open := true
	e.InspectPR = func(context.Context, string, string) (Item, error) { return Item{Open: open}, nil }
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	state.Runs = []Run{{ID: "activation", RequestID: "task", Status: "running", Workflow: w,
		Item: Item{Kind: "pr", Repository: w.Repository}, StartedAt: &start, Outcome: &Outcome{
			Status: "succeeded", CompletedAt: now, PRURL: "https://github.com/acme/repo/pull/7",
		}}}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if strings.Contains(timing.String(), "stage=workflow_total") {
		t.Fatalf("workflow total was emitted while checks were pending:\n%s", timing.String())
	}
	now = start.Add(time.Minute)
	open = false
	tick(t, e)
	spans, err := tasktiming.Read(t.Context(), strings.NewReader(timing.String()), "task")
	if err != nil || len(spans) != 2 || spans[0].Stage != tasktiming.Checks || spans[1].Stage != tasktiming.WorkflowTotal ||
		spans[0].EndedAt == nil || !spans[0].EndedAt.Equal(now) || spans[1].EndedAt == nil || !spans[1].EndedAt.Equal(now) ||
		spans[1].WallMS == nil || *spans[1].WallMS != 60000 ||
		!strings.Contains(aggregate.String(), "status=completed reason=completed duration_ms=60000") {
		t.Fatalf("terminal total and check wait disagree: %+v, err = %v\n%s\n%s", spans, err, timing.String(), aggregate.String())
	}
}

func TestWorkflowRetryClosesWaitAtQueueAcceptance(t *testing.T) {
	t.Parallel()
	w := definition("delivery")
	e, _, _ := testEngine(t, w)
	const id, requestID = "activation", "task"
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	now := start
	e.Now = func() time.Time { return now }
	var log strings.Builder
	e.TimingLog = func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	wait := tasktiming.Start(id, "wait", tasktiming.Checks, start)
	wait.Write(e.TimingLog, requestID)
	state.Runs = []Run{{ID: id, RequestID: requestID, Status: OutcomeChecksPending, Workflow: w,
		Outcome: &Outcome{Status: OutcomeChecksPending}, TimingWait: &wait}}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}

	now = start.Add(time.Minute)
	acceptedAt := now
	ctx := tasktiming.WithRecorder(t.Context(), requestID, "retry", e.now, e.TimingLog)
	if err := e.AcceptTimingAttempt(id, requestID, "retry", acceptedAt); err != nil {
		t.Fatal(err)
	}
	tasktiming.Accepted(ctx, acceptedAt)
	tick(t, e)
	spans, err := tasktiming.Read(t.Context(), strings.NewReader(log.String()), requestID)
	if err != nil || len(spans) != 2 || spans[0].Attempt != id || spans[0].EndedAt == nil ||
		!spans[0].EndedAt.Equal(acceptedAt) || spans[1].Stage != tasktiming.Queue || spans[1].Attempt != "retry" {
		t.Fatalf("retry acceptance did not close previous wait: %+v, err = %v\n%s", spans, err, log.String())
	}

	now = now.Add(3 * time.Second)
	tasktiming.Set(ctx, "")
	if err := e.Complete(id, Outcome{Status: OutcomeChecksPending, TimingAttempt: "retry", TimingAcceptedAt: acceptedAt}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	spans, err = tasktiming.Read(t.Context(), strings.NewReader(log.String()), requestID)
	if err != nil || len(spans) != 3 || spans[2].Attempt != "retry" || spans[2].Stage != tasktiming.Checks ||
		spans[2].StartedAt == nil || !spans[2].StartedAt.Equal(now) || spans[2].EndedAt != nil {
		t.Fatalf("new wait did not join retry timeline: %+v, err = %v\n%s", spans, err, log.String())
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
		if err := e.AcceptTimingAttempt(retry.ID, "task", "second-attempt", now); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
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

func TestFastWorkflowRetryClosesPriorWaitAtAcceptance(t *testing.T) {
	t.Parallel()
	w := definition("delivery")
	w.Merge.Enabled = true
	e, _, _ := testEngine(t, w)
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	acceptedAt := start.Add(10 * time.Second)
	now := acceptedAt
	e.Now = func() time.Time { return now }
	var log strings.Builder
	e.TimingLog = func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }
	item := Item{Repository: w.Repository, Kind: "issue", Number: 7}
	wait := tasktiming.Start("first-attempt", "wait", tasktiming.WorkflowWait, start)
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	state.Runs = []Run{{ID: "first", RequestID: "task", Status: awaitingReview, Workflow: w, Item: item,
		StartedAt: &start, TimingWait: &wait}}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}
	requestID, err := e.DispatchRetry(t.Context(), w.ID, w.ExecutionPolicyHash(), "first", item, nil, func(retry Run) (string, error) {
		now = acceptedAt.Add(3 * time.Second)
		err := e.Complete(retry.ID, Outcome{Status: "succeeded", TimingAttempt: "second-attempt", TimingAcceptedAt: acceptedAt})
		now = now.Add(time.Second)
		return "task", err
	})
	if err != nil || requestID != "task" {
		t.Fatalf("retry = %q, %v", requestID, err)
	}
	spans, err := tasktiming.Read(t.Context(), strings.NewReader(log.String()), requestID)
	if err != nil || len(spans) != 1 || spans[0].ID != wait.ID || spans[0].EndedAt == nil || !spans[0].EndedAt.Equal(acceptedAt) ||
		spans[0].WallMS == nil || *spans[0].WallMS != 10000 {
		t.Fatalf("prior wait did not close at retry acceptance: %+v, err = %v\n%s", spans, err, log.String())
	}
}
