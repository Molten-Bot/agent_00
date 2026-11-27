package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTriggerTransitionsWhileTaskIsRunning(t *testing.T) {
	t.Parallel()
	for _, event := range Events {
		if event == "schedule" || IsInputEvent(event) {
			continue
		}
		t.Run(event, func(t *testing.T) {
			t.Parallel()
			w := definition("transitions")
			w.Trigger = Trigger{Event: event, Value: "agent"}
			if event == ActionFailedEvent {
				w.Trigger.Value = "build.yml"
			}
			if strings.HasPrefix(event, "pr.") {
				w.Task.Kind = "review"
			}
			e, items, count := testEngine(t, w)
			tick(t, e) // Baseline before the item exists.
			item := Item{Repository: w.Repository, Number: 1, Kind: strings.Split(event, ".")[0], Open: true, Labels: []string{"agent"}, Assignees: []string{"agent"}, Reviewers: []string{"agent"}, ChecksFailed: true, ReviewDecision: "CHANGES_REQUESTED"}
			if event == ActionFailedEvent {
				item = Item{Repository: w.Repository, Kind: "action", ActionRunID: 1, ActionRunAttempt: 1, ChecksFailed: true}
			}
			*items = []Item{item}
			tick(t, e)
			if *count != 1 {
				t.Fatalf("first transition submitted %d tasks", *count)
			}
			// Observe the trigger becoming false while its first task still runs.
			switch event {
			case "issue.opened", "pr.opened":
				(*items)[0].Open = false
			case "issue.assigned", "pr.assigned":
				(*items)[0].Assignees = nil
			case "issue.labeled", "pr.labeled":
				(*items)[0].Labels = nil
			case "pr.ready":
				(*items)[0].Draft = true
			case "pr.review_requested":
				(*items)[0].Reviewers = nil
			case "pr.changes_requested":
				(*items)[0].ReviewDecision = "APPROVED"
			case "pr.checks_failed":
				(*items)[0].ChecksFailed = false
			case ActionFailedEvent:
				item.ActionRunAttempt++
				(*items)[0] = item
				(*items)[0].ChecksFailed = false
			}
			tick(t, e)
			*items = []Item{item}
			tick(t, e)
			if *count != 1 {
				t.Fatal("overlapped work on the same item")
			}
			runs, err := e.Runs()
			if err != nil {
				t.Fatal(err)
			}
			activeID := ""
			for _, run := range runs {
				if run.Status == "running" {
					activeID = run.ID
				}
			}
			if err = e.Complete(activeID, Outcome{Status: "succeeded"}); err != nil {
				t.Fatal(err)
			}
			tick(t, e)
			if *count != 2 {
				t.Fatalf("observed transition lost while busy: got %d tasks", *count)
			}
			restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch, Submit: e.Submit, Status: e.Status}
			tick(t, restarted)
			if *count != 2 {
				t.Fatal("restart duplicated the deferred transition")
			}
		})
	}
}

func TestMergeWaitsForNonemptyHead(t *testing.T) {
	t.Parallel()
	w := definition("merge")
	w.IncludeBacklog = true
	w.Merge.Enabled = true
	e, items, _ := testEngine(t, w)
	*items = []Item{{Repository: w.Repository, Number: 1, Kind: "issue", Open: true, Labels: []string{"ready"}}}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Complete(runs[0].ID, Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/2", ReviewReceipt: fixtureRunReviewReceipt(runs[0], "https://github.com/acme/repo/pull/2")}); err != nil {
		t.Fatal(err)
	}
	pr := Item{Open: true, ChecksPassed: true, HumanApproved: true}
	e.InspectPR = func(context.Context, string, string) (Item, error) { return pr, nil }
	merged := 0
	e.Merge = func(context.Context, Run) error { merged++; return nil }
	tick(t, e)
	if merged != 0 {
		t.Fatal("merged without a head commit")
	}
	runs, err = e.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != "waiting" || !strings.Contains(runs[0].Reason, "head commit") {
		t.Fatalf("missing diagnostic: %+v", runs[0])
	}
	pr.HeadSHA = "abc"
	tick(t, e)
	if merged != 1 {
		t.Fatal("did not merge after obtaining the head commit")
	}
}

func TestClosedThenReopenedItemCreatesNewActivation(t *testing.T) {
	t.Parallel()
	w := definition("reopened")
	w.Trigger = Trigger{Event: "issue.opened"}
	w.IncludeBacklog = true
	e, items, count := testEngine(t, w)
	item := Item{Repository: w.Repository, Number: 1, Kind: "issue", Open: true}
	*items = []Item{item}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Complete(runs[0].ID, Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	// GitHub polling returns only open items, so closure is an absent item.
	*items = nil
	tick(t, e)
	*items = []Item{item}
	tick(t, e)
	tick(t, e)
	if *count != 2 {
		t.Fatalf("closed/reopened item submitted %d tasks, want 2", *count)
	}
}

func TestFailedPollDoesNotResetObservations(t *testing.T) {
	t.Parallel()
	w := definition("poll-failure")
	w.IncludeBacklog = true
	e, items, count := testEngine(t, w)
	*items = []Item{{Repository: w.Repository, Number: 1, Kind: "issue", Open: true, Labels: []string{"ready"}}}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Complete(runs[0].ID, Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	fetch := e.Fetch
	e.Fetch = func(context.Context, string, ScanNeeds) ([]Item, error) {
		return nil, errors.New("temporary GitHub failure")
	}
	if err = e.Tick(context.Background()); err == nil {
		t.Fatal("missing poll error")
	}
	e.Fetch = fetch
	tick(t, e)
	if *count != 1 {
		t.Fatal("failed poll caused a duplicate activation")
	}
}
