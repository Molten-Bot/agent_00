package workflow

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The callback stays blocked until release or test cancellation.
func blockPoll(t *testing.T) (wait func(), entered <-chan struct{}, release func()) {
	t.Helper()
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	return func() {
		once.Do(func() { close(started) })
		select {
		case <-resume:
		case <-t.Context().Done():
		}
	}, started, sync.OnceFunc(func() { close(resume) })
}

func awaitPoll[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("operation blocked")
		panic("unreachable")
	}
}

func TestCompletionAndInputDuringPausedFetch(t *testing.T) {
	w := definition("issues")
	w.IncludeBacklog = true
	input := definition("input")
	input.Trigger = Trigger{Event: "app.prompt"}
	e, _, count := testEngine(t, scheduledDefinition(), w, input)
	var first Run
	if _, err := e.DispatchInput(t.Context(), input, func(r Run) (string, error) { first = r; return "first", nil }); err != nil {
		t.Fatal(err)
	}
	wait, entered, release := blockPoll(t)
	defer release()
	e.Fetch = func(context.Context, string, ScanNeeds) ([]Item, error) {
		wait()
		return []Item{{Number: 1, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}}}, nil
	}
	tickDone := make(chan error, 1)
	go func() { tickDone <- e.Tick(t.Context()) }()
	awaitPoll(t, entered)
	local := make(chan error, 1)
	var second Run
	go func() {
		statuses, err := e.ScheduleStatus()
		if err == nil && (len(statuses) != 1 || statuses[0].NextRunAt.IsZero()) {
			t.Error("missing committed schedule cursor", statuses)
		}
		if err == nil {
			err = e.Complete(first.ID, Outcome{Status: "succeeded"})
		}
		if err == nil {
			_, err = e.DispatchInput(t.Context(), input, func(r Run) (string, error) {
				second = r
				return "second", e.Complete(r.ID, Outcome{Status: "failed", Reason: "during submit"})
			})
		}
		local <- err
	}()
	if err := awaitPoll(t, local); err != nil {
		t.Fatal(err)
	}
	doc, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows[1].Enabled = false
	if _, err = e.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	release()
	if err := awaitPoll(t, tickDone); err != nil {
		t.Fatal(err)
	}
	s, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	a, b := s.run(first.ID), s.run(second.ID)
	if *count != 0 || a == nil || a.Outcome == nil || a.Outcome.Status != "succeeded" || b == nil || b.RequestID != "second" || b.Outcome == nil || b.Outcome.Reason != "during submit" {
		t.Fatalf("submits=%d runs=%+v", *count, s.Runs)
	}
	doc, err = e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows[1].Enabled = true
	if _, err = e.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	// Both scans see the same match after resuming.
	go func() { tickDone <- e.Tick(t.Context()) }()
	go func() { tickDone <- e.Tick(t.Context()) }()
	for range 2 {
		if err := awaitPoll(t, tickDone); err != nil {
			t.Fatal(err)
		}
	}
	if *count != 1 {
		t.Fatalf("submissions=%d, want one", *count)
	}
}

func TestDisableMergeDuringInspection(t *testing.T) {
	w := definition("merge")
	w.Merge.Enabled = true
	e, _, _ := testEngine(t, w)
	if err := e.transaction(func(s *State) error {
		s.Runs = append(s.Runs, Run{ID: "waiting", Workflow: w, Item: Item{Repository: w.Repository}, Status: "waiting", PRURL: "https://github.com/acme/repo/pull/1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wait, entered, release := blockPoll(t)
	defer release()
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		wait()
		return Item{Open: true, HeadSHA: "abc", ChecksPassed: true, HumanApproved: true}, nil
	}
	merges := 0
	e.Merge = func(context.Context, Run) error { merges++; return nil }
	done := make(chan error, 1)
	go func() { done <- e.Tick(t.Context()) }()
	awaitPoll(t, entered)
	doc, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows[0].Merge.Enabled = false
	if _, err = e.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	release()
	if err := awaitPoll(t, done); err != nil {
		t.Fatal(err)
	}
	if merges != 0 {
		t.Fatalf("merged after policy disabled")
	}
}

func TestCompletionWinsOverInFlightStatus(t *testing.T) {
	w := definition("status")
	e, _, _ := testEngine(t, w)
	if err := e.transaction(func(s *State) error {
		s.Runs = append(s.Runs, Run{ID: "activation", Workflow: w, Status: "running", RequestID: "request"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wait, entered, release := blockPoll(t)
	defer release()
	e.Status = func(string) Outcome { wait(); return Outcome{Status: "failed"} }
	done := make(chan error, 1)
	go func() { done <- e.Tick(t.Context()) }()
	awaitPoll(t, entered)
	complete := make(chan error, 1)
	go func() { complete <- e.Complete("activation", Outcome{Status: "succeeded"}) }()
	if err := awaitPoll(t, complete); err != nil {
		t.Fatal(err)
	}
	release()
	if err := awaitPoll(t, done); err != nil {
		t.Fatal(err)
	}
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Outcome == nil || runs[0].Outcome.Status != "succeeded" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}
