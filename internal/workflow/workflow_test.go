package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
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
func fixtureReviewReceipt(w Definition, prURL string) *githubutil.ReviewReceipt {
	passes := w.ReviewCycles
	if w.Task.Kind == "review" {
		passes = 0
	}
	return &githubutil.ReviewReceipt{
		PRURL: prURL, Head: "abc", Run: "run", Pass: 1, ReviewID: 42,
		Marker: "<!-- test review -->", URL: prURL + "#pullrequestreview-42",
		Policy: githubutil.ReviewPolicy{FixPasses: passes, Hash: w.ExecutionPolicyHash()},
		Result: json.RawMessage(`{"status":"clean","mergeReady":true,"findings":[]}`),
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
			outcome := Outcome{Status: "succeeded", PRURL: pr.URL}
			if clean {
				outcome.ReviewReceipt = fixtureReviewReceipt(w, pr.URL)
			}
			if err := e.Complete(runs[0].ID, outcome); err != nil {
				t.Fatal(err)
			}
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

func TestEngineReconcilesLateReviewReceipt(t *testing.T) {
	t.Parallel()
	w := definition("review")
	w.Trigger = Trigger{Event: "pr.labeled", Value: "review"}
	w.Task.Kind = "review"
	w.Merge.Enabled, w.IncludeBacklog = true, true
	e, items, submitted := testEngine(t, w)
	pr := Item{
		Number: 1, Repository: w.Repository, Kind: "pr", Open: true, Labels: []string{"review"},
		URL: "https://github.com/acme/repo/pull/1", HeadSHA: "old", ChecksPassed: true, HumanApproved: true,
	}
	*items = []Item{pr}
	inspections, merges := 0, 0
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		inspections++
		return pr, nil
	}
	e.Merge = func(_ context.Context, r Run) error {
		if r.MergeHead != pr.HeadSHA {
			t.Fatalf("merge head = %q, want %q", r.MergeHead, pr.HeadSHA)
		}
		merges++
		pr.Open, pr.Merged = false, true
		return nil
	}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("initial runs = %+v, error = %v", runs, err)
	}
	id := runs[0].ID
	outcome := Outcome{Status: "succeeded", PRURL: pr.URL}
	if err := e.Complete(id, outcome); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	runs, err = e.Runs()
	if err != nil || len(runs) != 1 || runs[0].ID != id || runs[0].MergeHead != "" || inspections != 1 || merges != 0 {
		t.Fatalf("awaiting review: runs = %+v, inspections = %d, merges = %d, error = %v", runs, inspections, merges, err)
	}

	// A fresh trigger must still dispatch while the earlier activation waits.
	(*items)[0].Labels = nil
	tick(t, e)
	(*items)[0].Labels = []string{"review"}
	tick(t, e)
	if *submitted != 2 {
		t.Fatalf("submissions = %d, want 2", *submitted)
	}

	pr.HeadSHA = "new"
	outcome.ReviewReceipt = fixtureReviewReceipt(w, pr.URL)
	if err := e.Complete(id, outcome); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if merges != 0 {
		t.Fatal("merged with a stale receipt")
	}
	freshReceipt := *outcome.ReviewReceipt
	freshReceipt.Head = pr.HeadSHA
	outcome.ReviewReceipt = &freshReceipt
	if err := e.Complete(id, outcome); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	tick(t, e)
	if merges != 1 {
		t.Fatalf("merges = %d, want 1", merges)
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

func TestImplementationMergeRequiresPersistedReceiptOnCurrentHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, head                              string
		receipt, interrupted, merge             bool
		checksPending, unresolved, queueEnabled bool
		requireApproval                         bool
		requireCheck, checkPublished            bool
	}{
		{name: "no receipt", head: "abc"},
		{name: "matching receipt", head: "abc", receipt: true, merge: true},
		{name: "CI pending", head: "abc", receipt: true, checksPending: true},
		{name: "unresolved thread", head: "abc", receipt: true, unresolved: true},
		{name: "merge queue enabled", head: "abc", receipt: true, queueEnabled: true},
		{name: "human approval required", head: "abc", receipt: true, requireApproval: true},
		{name: "trusted check pending", head: "abc", receipt: true, requireCheck: true},
		{name: "trusted check published", head: "abc", receipt: true, requireCheck: true, checkPublished: true, merge: true},
		{name: "changed head", head: "def", receipt: true},
		{name: "interrupted", head: "abc", receipt: true, interrupted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := definition("implementation")
			w.Merge.Enabled, w.IncludeBacklog = true, true
			w.Merge.DeleteBranch, w.Merge.RequireApproval = true, tc.requireApproval
			w.ReviewCycles = 10
			w.ReviewAgent.Complexity = "easy"
			e, items, _ := testEngine(t, w)
			*items = []Item{{Kind: "issue", Number: 1, Repository: w.Repository, Open: true, Labels: []string{"ready"}}}
			tick(t, e)
			runs, err := e.Runs()
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs = %+v, %v", runs, err)
			}
			o := Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/12", ReviewInterrupted: tc.interrupted}
			if tc.receipt {
				o.ReviewReceipt = fixtureReviewReceipt(runs[0].Workflow, o.PRURL)
				if tc.requireCheck {
					o.ReviewReceipt.Policy.CheckAppID = 456
				}
			}
			if err := e.Complete(runs[0].ID, o); err != nil {
				t.Fatal(err)
			}
			merges := 0
			prState := "OPEN"
			gh := GitHub{Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				reply := func(raw string) (execx.Result, error) { return execx.Result{Stdout: raw}, nil }
				args := strings.Join(cmd.Args, " ")
				switch {
				case strings.HasPrefix(args, "pr view "):
					ci := "SUCCESS"
					if tc.checksPending {
						ci = ""
					}
					return reply(fmt.Sprintf(`{"number":12,"state":%q,"headRefOid":%q,"baseRefName":"main","reviewDecision":null,"statusCheckRollup":[{"name":"CI","conclusion":%q}]}`, prState, tc.head, ci))
				case strings.Contains(args, "closingIssuesReferences"):
					return reply("0")
				case strings.Contains(args, "/rules/branches/main?"):
					return reply(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"agent00 / review-policy","integration_id":456}]}},{"type":"pull_request","parameters":{"required_review_thread_resolution":true}}]`)
				case strings.Contains(args, "/commits/abc/check-runs?"):
					if tc.checkPublished {
						return reply(fmt.Sprintf(`{"check_runs":[{"id":123,"name":"agent00 / review-policy","head_sha":"abc","external_id":"pr-12-review-%d","status":"completed","conclusion":"success","app":{"id":456}}]}`, o.ReviewReceipt.ReviewID))
					}
					return reply(`{"check_runs":[]}`)
				case strings.Contains(args, "/reviews?"):
					receipt := o.ReviewReceipt
					if receipt == nil {
						return reply("[]")
					}
					reviews := []githubutil.SubmittedReview{{
						ID: receipt.ReviewID, Head: receipt.Head, URL: receipt.URL, State: "COMMENTED", SubmittedAt: "2026-11-25T12:00:00Z",
						Body: receipt.Marker + "\n" + githubutil.CleanReviewVerdict + "\nNo material issues found.",
					}}
					data, err := json.Marshal(reviews)
					return execx.Result{Stdout: string(data)}, err
				case strings.Contains(args, "reviewThreads(first:100"):
					threads := "[]"
					if tc.unresolved {
						threads = `[{"id":"old","isResolved":false}]`
					}
					return reply(fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":%q,"isMergeQueueEnabled":%t,"reviewThreads":{"nodes":%s,"pageInfo":{"hasNextPage":false}}}}}}`, tc.head, tc.queueEnabled, threads))
				case strings.HasPrefix(args, "pr merge "):
					if !strings.Contains(args, "--match-head-commit abc") || slices.Contains(cmd.Args, "--admin") || slices.Contains(cmd.Args, "--auto") {
						t.Errorf("unexpected gh merge args: %v", cmd.Args)
					}
					merges++
					prState = "MERGED"
					return reply("")
				default:
					t.Fatalf("unexpected GitHub command: %v", cmd.Args)
					return execx.Result{}, nil
				}
			})}
			restarted := &Engine{
				Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed,
				Fetch: e.Fetch, Submit: e.Submit, Status: e.Status,
				InspectPR: gh.Inspect, Merge: gh.Merge,
			}
			tick(t, restarted)
			tick(t, restarted)
			if (merges == 1) != tc.merge || merges > 1 {
				t.Fatalf("merge attempts = %d", merges)
			}
		})
	}
}
