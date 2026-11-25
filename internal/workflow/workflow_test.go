package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func definition(id string) Definition {
	return Definition{ID: id, Name: "Implement issues", Repository: "acme/repo", Enabled: true, Trigger: Trigger{Event: "issue.labeled", Value: "ready"}, Filters: Filters{Mode: "all", Rules: []Rule{}}, Task: Task{Kind: "implement", Instructions: "Implement the issue", IssueLinkMode: "reference"}, Merge: Merge{Method: "squash", RequireApproval: true}}
}
func TestStoreVersionAndConflict(t *testing.T) {
	t.Parallel()
	s := &Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	d, err := s.Load()
	if err != nil || len(d.Workflows) != 0 {
		t.Fatal(d, err)
	}
	d.Workflows = []Definition{definition("one"), definition("two")}
	saved, err := s.Save(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(d); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	round, err := s.Load()
	if err != nil || len(round.Workflows) != 2 || round.Revision != saved.Revision {
		t.Fatal(round, err)
	}
	future := []byte(`{"version":99,"revision":0,"workflows":[]}`)
	os.WriteFile(s.Path, future, 0600)
	if _, err = s.Load(); err == nil {
		t.Fatal("accepted future version")
	}
	if _, err = s.Save(saved); err == nil {
		t.Fatal("overwrote future version")
	}
	data, _ := os.ReadFile(s.Path)
	if string(data) != string(future) {
		t.Fatal("future file modified")
	}
}
func TestDefinitionValidation(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*Definition){func(w *Definition) { w.Repository = "../bad" }, func(w *Definition) { w.ReviewCycles = 11 }, func(w *Definition) { w.Trigger.Event = "unknown" }, func(w *Definition) { w.Trigger.Value = "" }, func(w *Definition) { w.Merge.Method = "--admin" }, func(w *Definition) { w.Merge.Enabled = true; w.Task.Draft = true }, func(w *Definition) { w.Filters.Rules = []Rule{{Field: "draft", Operator: "is", Value: "maybe"}} }} {
		w := definition("one")
		change(&w)
		if w.Validate() == nil {
			t.Fatalf("accepted invalid definition %+v", w)
		}
	}
	if _, err := Decode([]byte(`{"version":1,"revision":0,"workflows":[],"unknown":true}`)); err == nil {
		t.Fatal("ignored unknown field")
	}
}
func TestFiltersAndTriggers(t *testing.T) {
	t.Parallel()
	i := Item{Repository: "acme/repo", Kind: "issue", Open: true, Labels: []string{"Ready", "bug"}, Assignees: []string{"agent"}, Viewer: "agent", IssueType: "Bug"}
	w := definition("one")
	w.Filters.Rules = []Rule{{Field: "assignee", Operator: "is", Value: "@me"}, {Field: "issueType", Operator: "is", Value: "Bug"}, {Field: "label", Operator: "isNot", Value: "blocked"}}
	if ok, why := Match(w, i); !ok {
		t.Fatal(why)
	}
	i.Labels = append(i.Labels, "blocked")
	if ok, _ := Match(w, i); ok {
		t.Fatal("ignored exclusion")
	}
	w.Filters.Mode = "any"
	if ok, _ := Match(w, i); !ok {
		t.Fatal("any rule ignored")
	}
	for _, event := range Events {
		if event == "schedule" || IsInputEvent(event) {
			continue
		}
		w.Trigger = Trigger{Event: event, Value: "agent"}
		i.Kind = strings.Split(event, ".")[0]
		i.Labels = []string{"agent"}
		i.Reviewers = []string{"agent"}
		i.ReviewDecision = "CHANGES_REQUESTED"
		i.ChecksFailed = true
		w.Filters.Rules = nil
		if ok, why := Match(w, i); !ok {
			t.Fatalf("%s: %s", event, why)
		}
	}
}
func testEngine(t *testing.T, w ...Definition) (*Engine, *[]Item, *int) {
	t.Helper()
	root := t.TempDir()
	store := &Store{Path: filepath.Join(root, "workflows.json")}
	if err := store.Initialize(Document{Version: Version, Workflows: w}); err != nil {
		t.Fatal(err)
	}
	items := []Item{}
	submitted := 0
	e := &Engine{Store: store, StatePath: filepath.Join(root, "state.json"), Enabled: func() bool { return true }, Allowed: func(string) bool { return true }, Fetch: func(context.Context, string, ScanNeeds) ([]Item, error) { return items, nil }, Submit: func(context.Context, Run) (string, error) { submitted++; return "task", nil }, Status: func(string) Outcome { return Outcome{Status: "running"} }}
	return e, &items, &submitted
}
func tick(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestEngineBaselineDedupeRestartAndFilters(t *testing.T) {
	t.Parallel()
	e, items, count := testEngine(t, definition("one"))
	*items = []Item{{Number: 1, Repository: "acme/repo", Kind: "issue", Open: true, Labels: []string{"ready"}}}
	tick(t, e)
	if *count != 0 {
		t.Fatal("backlog ran")
	}
	*items = append(*items, Item{Number: 2, Repository: "acme/repo", Kind: "issue", Open: true, Labels: []string{"ready"}})
	tick(t, e)
	tick(t, e)
	if *count != 1 {
		t.Fatal("activation was lost or duplicated", *count)
	}
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch, Submit: e.Submit, Status: e.Status}
	tick(t, restarted)
	if *count != 1 {
		t.Fatal("restart duplicated activation")
	}
	runs, _ := e.Runs()
	if len(runs) != 1 || runs[0].Item.Number != 2 {
		t.Fatal(runs)
	}
	e.Enabled = func() bool { return false }
	*items = append(*items, Item{Number: 3, Repository: "acme/repo", Kind: "issue", Open: true, Labels: []string{"ready"}})
	tick(t, e)
	if *count != 1 {
		t.Fatal("feature gate ignored")
	}
}

func TestTickRemovesLegacyHistoryWhileKeepingPendingRuns(t *testing.T) {
	t.Parallel()
	w := definition("history-upgrade")
	e, _, _ := testEngine(t, w)
	e.Enabled = func() bool { return false }
	s, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	s.Observations["baseline:acme/repo#1"] = observation{Fingerprint: "issue.labeled", Generation: 2}
	s.Baselines["baseline"] = true
	s.Schedules = map[string]ScheduleCursor{w.ID: {Policy: "schedule-policy"}}
	for _, status := range []string{"failed", "succeeded", "merged", "completed", "dispatching", "running", "waiting"} {
		s.Runs = append(s.Runs, Run{ID: status, Workflow: w, Item: Item{Repository: w.Repository}, Status: status})
	}
	w.Merge.Enabled = true
	doc, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows = []Definition{w}
	if _, err := e.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(e.StatePath, s); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	got, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 3 || got.Runs[0].ID != "dispatching" || got.Runs[1].ID != "running" || got.Runs[2].ID != "waiting" {
		t.Fatalf("retained activations: %+v", got.Runs)
	}
	if got.Observations["baseline:acme/repo#1"].Generation != 2 || !got.Baselines["baseline"] || got.Schedules[w.ID].Policy != "schedule-policy" {
		t.Fatalf("lost operational state: %+v", got)
	}
}
func TestEngineMultipleWorkflowsSerializeEntity(t *testing.T) {
	t.Parallel()
	a, b := definition("one"), definition("two")
	a.IncludeBacklog = true
	b.IncludeBacklog = true
	e, items, count := testEngine(t, a, b)
	*items = []Item{{Number: 1, Repository: "acme/repo", Kind: "issue", Open: true, Labels: []string{"ready"}}}
	tick(t, e)
	if *count != 1 {
		t.Fatal("concurrent entity work", *count)
	}
	runs, _ := e.Runs()
	if err := e.Complete(runs[0].ID, Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if *count != 2 {
		t.Fatal("second independent workflow did not run", *count)
	}
}
func TestEngineMergeGatesAndReviewFindings(t *testing.T) {
	t.Parallel()
	for _, clean := range []bool{false, true} {
		t.Run(map[bool]string{false: "findings", true: "clean"}[clean], func(t *testing.T) {
			w := definition("review")
			w.Trigger = Trigger{Event: "pr.opened"}
			w.Task.Kind = "review"
			w.Merge.Enabled = true
			w.IncludeBacklog = true
			e, items, _ := testEngine(t, w)
			*items = []Item{{Number: 1, Repository: w.Repository, Kind: "pr", Open: true, URL: "https://github.com/acme/repo/pull/1", HeadSHA: "abc"}}
			merged := 0
			pr := (*items)[0]
			e.InspectPR = func(context.Context, string, string) (Item, error) { return pr, nil }
			e.Merge = func(context.Context, Run) error { merged++; return nil }
			tick(t, e)
			runs, _ := e.Runs()
			e.Complete(runs[0].ID, Outcome{Status: "succeeded", ReviewClean: clean, ReviewedHead: "abc", PRURL: pr.URL})
			tick(t, e)
			if merged != 0 {
				t.Fatal("merged without checks")
			}
			pr.ChecksPassed = true
			tick(t, e)
			if merged != 0 {
				t.Fatal("merged without approval")
			}
			pr.ReviewDecision = "APPROVED"
			pr.HumanApproved = true
			tick(t, e)
			want := 0
			if clean {
				want = 1
			}
			if merged != want {
				t.Fatalf("merge count %d, want %d", merged, want)
			}
			tick(t, e)
			if merged != want {
				t.Fatal("duplicate merge")
			}
		})
	}
}
func TestUncertainSubmissionNeverAutomaticallyRetries(t *testing.T) {
	t.Parallel()
	w := definition("one")
	w.IncludeBacklog = true
	e, items, count := testEngine(t, w)
	*items = []Item{{Number: 1, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}}}
	tick(t, e)
	s, _ := e.load()
	s.Runs[0].Status = "dispatching"
	s.Runs[0].RequestID = ""
	writeAtomic(e.StatePath, s)
	tick(t, e)
	if *count != 1 {
		t.Fatal("uncertain dispatch duplicated")
	}
	e.Reconcile = func(string) string { return "original-task" }
	tick(t, e)
	runs, _ := e.Runs()
	if runs[0].RequestID != "original-task" {
		t.Fatal("did not reconcile")
	}
}

type fakeRunner func(context.Context, execx.Command) (execx.Result, error)

func (f fakeRunner) Run(c context.Context, cmd execx.Command) (execx.Result, error) { return f(c, cmd) }
func TestGitHubPollingAndHeadBoundMerge(t *testing.T) {
	t.Parallel()
	var commands []execx.Command
	g := GitHub{Runner: fakeRunner(func(_ context.Context, c execx.Command) (execx.Result, error) {
		commands = append(commands, c)
		args := strings.Join(c.Args, " ")
		if strings.Contains(args, "api graphql") {
			return execx.Result{Stdout: "0\n"}, nil
		}
		if strings.Contains(args, "/reviews?") {
			return execx.Result{Stdout: `[]`}, nil
		}
		if strings.Contains(args, "repos/") {
			return execx.Result{Stdout: `[{"number":1,"title":"Bug","html_url":"https://github.com/acme/repo/issues/1","user":{"login":"author"},"type":{"name":"Bug"},"labels":[{"name":"ready"}]},{"number":2,"pull_request":{}}]`}, nil
		}
		if strings.Contains(args, "user") {
			return execx.Result{Stdout: "agent\n"}, nil
		}
		if strings.Contains(args, "pr view") {
			return execx.Result{Stdout: `{"number":2,"state":"OPEN","headRefOid":"abc","reviewDecision":"APPROVED","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}`}, nil
		}
		return execx.Result{}, nil
	})}
	items, err := g.Fetch(context.Background(), "acme/repo", ScanNeeds{Issues: true, PullRequests: true, Viewer: true, Reviews: true, LinkedIssues: true, Checks: true})
	if err != nil || len(items) != 2 || items[0].IssueType != "Bug" || items[0].Viewer != "agent" || !items[1].ChecksPassed {
		t.Fatal(items, err)
	}
	w := definition("one")
	w.Merge.DeleteBranch = true
	if err = g.Merge(context.Background(), Run{Workflow: w, Item: items[1], PRURL: "https://github.com/acme/repo/pull/2", MergeHead: "abc"}); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(commands[len(commands)-1].Args, " ")
	if !strings.Contains(args, "--match-head-commit abc") || !strings.Contains(args, "--squash") || !strings.Contains(args, "--delete-branch") || strings.Contains(args, "--admin") {
		t.Fatal(args)
	}
}
func TestStateFutureVersionFailsClosed(t *testing.T) {
	t.Parallel()
	e, _, count := testEngine(t, definition("one"))
	data, _ := json.Marshal(map[string]any{"version": 99})
	os.WriteFile(e.StatePath, data, 0600)
	if e.Tick(context.Background()) == nil || *count != 0 {
		t.Fatal("future state executed")
	}
}

func TestPausedOrDisallowedWorkflowDoesNotMerge(t *testing.T) {
	t.Parallel()
	w := definition("one")
	w.IncludeBacklog = true
	w.Merge.Enabled = true
	e, items, _ := testEngine(t, w)
	*items = []Item{{Number: 1, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}}}
	tick(t, e)
	runs, _ := e.Runs()
	e.Complete(runs[0].ID, Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/2"})
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		t.Fatal("inspected a paused workflow")
		return Item{}, nil
	}
	e.Allowed = func(string) bool { return false }
	tick(t, e)
	e.Allowed = func(string) bool { return true }
	doc, _ := e.Store.Load()
	doc.Workflows[0].Enabled = false
	e.Store.Save(doc)
	tick(t, e)
}
func TestPreviewHasNoSideEffects(t *testing.T) {
	t.Parallel()
	w := definition("one")
	e, items, count := testEngine(t, w)
	*items = []Item{{Number: 1, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}}}
	decisions, err := e.Preview(context.Background(), w)
	if err != nil || len(decisions) != 1 || !decisions[0].Matches || *count != 0 {
		t.Fatal(decisions, err)
	}
	if _, err := os.Stat(e.StatePath); !os.IsNotExist(err) {
		t.Fatal("preview wrote runtime state")
	}
}
func TestEditedDefinitionBaselinesWithoutReplayingBacklog(t *testing.T) {
	t.Parallel()
	w := definition("one")
	e, items, count := testEngine(t, w)
	*items = []Item{{Number: 1, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}}}
	tick(t, e)
	doc, _ := e.Store.Load()
	doc.Workflows[0].Task.Instructions = "Different instructions"
	e.Store.Save(doc)
	tick(t, e)
	if *count != 0 {
		t.Fatal("policy edit replayed backlog")
	}
}

func TestHumanApprovalRequiresTrustedHumanOnCurrentHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, association, head string
		want                          bool
	}{{"human", "User", "MEMBER", "abc", true}, {"bot", "Bot", "MEMBER", "abc", false}, {"outsider", "User", "CONTRIBUTOR", "abc", false}, {"stale", "User", "MEMBER", "old", false}} {
		t.Run(tc.name, func(t *testing.T) {
			i := Item{HeadSHA: "abc"}
			data, _ := json.Marshal([]any{map[string]any{"state": "APPROVED", "commit_id": tc.head, "author_association": tc.association, "user": map[string]string{"login": "reviewer", "type": tc.kind}}})
			if err := applyReviews(&i, string(data)); err != nil {
				t.Fatal(err)
			}
			if i.HumanApproved != tc.want {
				t.Fatalf("approval %v want %v", i.HumanApproved, tc.want)
			}
		})
	}
	i := Item{HeadSHA: "abc"}
	if err := applyReviews(&i, `[{"state":"APPROVED","commit_id":"abc","author_association":"MEMBER","user":{"login":"reviewer","type":"User"}},{"state":"CHANGES_REQUESTED","commit_id":"abc","author_association":"MEMBER","user":{"login":"reviewer","type":"User"}}]`); err != nil {
		t.Fatal(err)
	}
	if i.HumanApproved || i.ReviewDecision != "CHANGES_REQUESTED" {
		t.Fatal("later review did not supersede approval")
	}
}
