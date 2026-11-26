package workflow

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestActionTriggerValidation(t *testing.T) {
	t.Parallel()
	w := definition("release")
	w.Trigger.Event = ActionFailedEvent
	for _, selector := range []string{"", "all", ".github/workflows/deploy.yml", "../deploy.yml", "deploy.yml?status=failure", "0"} {
		w.Trigger.Value = selector
		if w.Validate() == nil {
			t.Errorf("accepted selector %q", selector)
		}
	}
	for _, selector := range []string{"deploy-prod.yml", "release.yaml", "12345"} {
		w.Trigger.Value = selector
		if err := w.Validate(); err != nil {
			t.Errorf("rejected selector %q: %v", selector, err)
		}
	}
	w.Filters.Rules = []Rule{{Field: "label", Operator: "is", Value: "release"}}
	if w.Validate() == nil {
		t.Error("accepted issue filters for Actions")
	}
	w.Filters.Rules = nil
	w.Task.Kind = "repair"
	if w.Validate() == nil {
		t.Error("accepted PR repair task for Actions")
	}
}

func TestActionFailurePollsSelectedWorkflowAndDeduplicatesAttempts(t *testing.T) {
	t.Parallel()
	release := definition("release")
	release.Trigger = Trigger{Event: ActionFailedEvent, Value: " deploy-prod.yml "}
	other := definition("other")
	other.Trigger = Trigger{Event: ActionFailedEvent, Value: "ci.yml"}
	other.IncludeBacklog = true
	e, _, _ := testEngine(t, release, other, definition("issues"))

	const releasePath = "repos/acme/repo/actions/workflows/deploy-prod.yml/runs?per_page=100"
	const otherPath = "repos/acme/repo/actions/workflows/ci.yml/runs?per_page=100"
	attempt, status := 1, "completed"
	g := GitHub{Runner: fakeRunner(func(_ context.Context, c execx.Command) (execx.Result, error) {
		switch {
		case slices.Equal(c.Args, []string{"api", releasePath}):
			return execx.Result{Stdout: fmt.Sprintf(`{"workflow_runs":[
				{"id":42000000001,"run_attempt":%d,"name":"Release","status":%q,"conclusion":"failure","head_branch":"main","head_sha":"abc"},
				{"id":42000000002,"run_attempt":1,"status":"completed","conclusion":"success"},
				{"id":42000000003,"run_attempt":1,"status":"completed","conclusion":"cancelled"},
				{"id":42000000004,"run_attempt":1,"status":"completed","conclusion":"failure","head_branch":"agent00-release-fix"}
			]}`, attempt, status)}, nil
		case slices.Equal(c.Args, []string{"api", otherPath}):
			return execx.Result{Stdout: `{"workflow_runs":[{"id":42000000005,"run_attempt":1,"status":"completed","conclusion":"failure"}]}`}, nil
		case slices.Contains(c.Args, "repos/acme/repo/issues?state=open&per_page=100"):
			return execx.Result{Stdout: "[]"}, nil
		default:
			t.Fatalf("unexpected GitHub command: %v", c.Args)
			return execx.Result{}, nil
		}
	})}
	e.Fetch = g.Fetch
	e.Status = func(string) Outcome { return Outcome{Status: "succeeded"} }
	var submitted []Run
	e.Submit = func(_ context.Context, r Run) (string, error) {
		submitted = append(submitted, r)
		return r.ID, nil
	}
	preview, err := e.Preview(t.Context(), release)
	if err != nil || len(preview) != 4 || !preview[0].Matches || preview[1].Matches || preview[2].Matches || preview[3].Matches {
		t.Fatalf("unexpected preview: %+v, %v", preview, err)
	}
	tick(t, e)
	if len(submitted) != 1 || submitted[0].Workflow.ID != other.ID {
		t.Fatalf("expected only the workflow with backlog enabled: %v", submitted)
	}
	attempt, status = 2, "in_progress"
	tick(t, e)
	if len(submitted) != 1 {
		t.Fatal("dispatched an in-progress attempt")
	}
	status = "completed"
	tick(t, e)
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch, Submit: e.Submit, Status: e.Status}
	tick(t, restarted)
	if len(submitted) != 2 || submitted[1].Workflow.ID != release.ID {
		t.Fatalf("expected one dispatch for the new failure attempt: %v", submitted)
	}
	item := submitted[1].Item
	if item.ActionRunID != 42000000001 || item.ActionRunAttempt != 2 || item.HeadSHA != "abc" || item.URL != "https://github.com/acme/repo/actions/runs/42000000001/attempts/2" {
		t.Fatalf("incorrect failure context: %+v", item)
	}
	// A rerun can also finish between polls without an observed in-progress state.
	attempt = 3
	tick(t, restarted)
	if len(submitted) != 3 || submitted[2].Item.ActionRunAttempt != 3 {
		t.Fatalf("missed consecutive failed attempt: %v", submitted)
	}
}

func TestActionFailureQueuedWhileBusySurvivesSuccessAndRestart(t *testing.T) {
	t.Parallel()
	w := definition("queued")
	w.Trigger = Trigger{Event: ActionFailedEvent, Value: "build.yml"}
	w.IncludeBacklog = true
	e, items, count := testEngine(t, w)
	*items = []Item{{Repository: w.Repository, Kind: "action", ActionRunID: 42, ActionRunAttempt: 1, ChecksFailed: true}}
	tick(t, e)
	runs, _ := e.Runs()
	first := runs[0].ID
	(*items)[0].ActionRunAttempt = 2
	tick(t, e)
	tick(t, e)
	if *count != 1 {
		t.Fatal("overlapping repairs", *count)
	}
	queued, err := e.Runs()
	if err != nil || len(queued) != 2 {
		t.Fatalf("repeated polls duplicated the pending activation: %+v, %v", queued, err)
	}
	(*items)[0].ActionRunAttempt = 3
	(*items)[0].ChecksFailed = false
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch, Submit: e.Submit, Status: e.Status}
	tick(t, restarted)
	if err := restarted.Complete(first, Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	tick(t, restarted)
	tick(t, restarted)
	if *count != 2 {
		t.Fatal("lost or duplicated queued attempt", *count)
	}
	runs, _ = restarted.Runs()
	found := false
	for _, r := range runs {
		if r.Item.ActionRunAttempt == 2 && r.Status == "running" {
			found = true
		}
	}
	if !found {
		t.Fatal("queued failure context was lost", runs)
	}
}
