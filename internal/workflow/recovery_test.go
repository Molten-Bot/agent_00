package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestOlderWorkflowJournalDoesNotTreatCleanFlagAsReceipt(t *testing.T) {
	t.Parallel()
	w := definition("legacy-review")
	w.ReviewCycles, w.Merge.Enabled = 1, true
	e, _, _ := testEngine(t, w)
	s, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	s.Runs = []Run{{ID: "legacy", Workflow: w, Item: Item{Repository: w.Repository}, Status: "running",
		Outcome: &Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/12", LegacyReviewClean: true, LegacyReviewedHead: "abc"},
	}}
	if err := writeAtomic(e.StatePath, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.StatePath)
	if err != nil || !strings.Contains(string(data), `"reviewClean": true`) {
		t.Fatalf("legacy evidence missing from fixture: %v", err)
	}
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		t.Fatal("legacy review flag authorized merge eligibility")
		return Item{}, nil
	}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 0 {
		t.Fatalf("legacy run was not retired: %+v, %v", runs, err)
	}
}

func TestReviewOutcomeSurvivesWorkspaceCleanup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const requestID = "local-delivery"
	w := definition("delivery")
	w.ReviewCycles = 10
	prURL := "https://github.com/acme/repo/pull/12"
	receipt := fixtureReviewReceipt(w, prURL)
	outcome := Outcome{Status: "succeeded", PRURL: prURL, ReviewReceipt: receipt, Reason: "task details"}
	if err := SaveReviewOutcome(root, requestID, outcome); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReviewOutcome(root, requestID)
	if err != nil || got.Status != "succeeded" || got.PRURL != prURL || got.Reason != "" ||
		got.ReviewReceipt == nil || !got.ReviewReceipt.Clean() || got.ReviewReceipt.ReviewID != receipt.ReviewID ||
		got.ReviewReceipt.Policy != receipt.Policy {
		t.Fatalf("archived outcome = %+v, err = %v", got, err)
	}
}

func TestDispatchRecoveryUsesSavedOutcomeWithoutTaskHistory(t *testing.T) {
	t.Parallel()
	w := definition("recover-outcome")
	e, _, _ := testEngine(t, w)
	var events []string
	e.Logf = func(format string, args ...any) { events = append(events, args[4].(string)) }
	e.Status = func(string) Outcome {
		t.Fatal("looked up task status despite durable outcome")
		return Outcome{}
	}
	s, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	s.Runs = []Run{{ID: "activation", Workflow: w, Item: Item{Repository: w.Repository}, Status: "dispatching"}}
	if err := e.save(s); err != nil {
		t.Fatal(err)
	}
	if err := e.Complete("activation", Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 0 || len(events) != 1 || events[0] != "succeeded" {
		t.Fatalf("runs=%+v events=%v err=%v", runs, events, err)
	}
}

func TestCompletedRunDoesNotReleaseAnotherRunOnSameItem(t *testing.T) {
	t.Parallel()
	w := definition("busy-item")
	w.IncludeBacklog = true
	e, items, count := testEngine(t, w)
	item := Item{Number: 1, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}}
	*items = []Item{item}
	s, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	s.Runs = []Run{
		{ID: "running", RequestID: "running", Workflow: w, Item: item, Status: "running"},
		{ID: "finished", Workflow: w, Item: item, Status: "running", Outcome: &Outcome{Status: "failed"}},
	}
	if err := e.save(s); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if *count != 0 {
		t.Fatal("overlapped a trigger with an active run")
	}
	if err := e.Complete("running", Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if *count != 1 {
		t.Fatalf("deferred trigger submissions=%d, want 1", *count)
	}
}

func TestMergeResponseLostReconcilesMergedPullRequest(t *testing.T) {
	t.Parallel()
	w := definition("merge-recovery")
	w.Merge.Enabled = true
	w.Merge.RequireApproval = false
	e, _, _ := testEngine(t, w)
	var lastStatus string
	e.Logf = func(format string, args ...any) { lastStatus = args[4].(string) }
	merges := 0
	prURL := "https://github.com/acme/repo/pull/42"
	g := GitHub{Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		args := strings.Join(cmd.Args, " ")
		switch {
		case strings.HasPrefix(args, "pr view "):
			state := "OPEN"
			if merges != 0 {
				state = "MERGED"
			}
			return execx.Result{Stdout: `{"number":42,"state":"` + state + `","headRefOid":"head","statusCheckRollup":[{"conclusion":"SUCCESS"}]}`}, nil
		case strings.HasPrefix(args, "api graphql"):
			return execx.Result{Stdout: "0"}, nil
		case strings.Contains(args, "/reviews?"):
			return execx.Result{Stdout: "[]"}, nil
		default:
			t.Fatalf("unexpected GitHub command %q", args)
			return execx.Result{}, nil
		}
	})}
	e.InspectPR = g.Inspect
	e.Merge = func(context.Context, Run) error {
		merges++
		return errors.New("connection dropped after GitHub accepted the merge")
	}
	s, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	s.Runs = []Run{{ID: "merge", Workflow: w, Item: Item{Repository: w.Repository}, PRURL: prURL, Status: "waiting"}}
	if err := e.save(s); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if merges != 1 || lastStatus != "waiting" {
		t.Fatalf("after timeout merges=%d status=%s", merges, lastStatus)
	}
	tick(t, e)
	runs, err := e.Runs()
	if merges != 1 || lastStatus != "merged" || err != nil || len(runs) != 0 {
		t.Fatalf("after reconciliation merges=%d status=%s runs=%+v err=%v", merges, lastStatus, runs, err)
	}
}

func TestTransientMergeFailureRetriesSameHead(t *testing.T) {
	t.Parallel()
	w := definition("merge-retry")
	w.Merge.Enabled = true
	w.Merge.RequireApproval = false
	e, _, _ := testEngine(t, w)
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		return Item{Open: true, ChecksPassed: true, HeadSHA: "reviewed-head"}, nil
	}
	var attemptedHeads []string
	e.Merge = func(_ context.Context, run Run) error {
		attemptedHeads = append(attemptedHeads, run.MergeHead)
		if len(attemptedHeads) == 1 {
			return errors.New("temporarily unavailable")
		}
		return nil
	}
	s, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	now := e.now()
	startedAt := now.Add(-time.Minute)
	s.Runs = []Run{{ID: "merge", Workflow: w, Item: Item{Repository: w.Repository}, PRURL: "https://github.com/acme/repo/pull/42", Status: "waiting", StartedAt: &startedAt}}
	if err := e.save(s); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Status != "waiting" || runs[0].MergeHead != "reviewed-head" || !strings.HasPrefix(runs[0].Reason, "Merge blocked:") {
		t.Fatalf("unexpected state after merge failure: runs=%+v err=%v", runs, err)
	}

	var events []string
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch, InspectPR: e.InspectPR, Merge: e.Merge,
		Now:  func() time.Time { return now.Add(time.Minute) },
		Logf: func(format string, args ...any) { events = append(events, fmt.Sprintf(format, args...)) },
	}
	tick(t, restarted)
	tick(t, restarted)
	runs, err = restarted.Runs()
	if err != nil || len(runs) != 0 || strings.Join(attemptedHeads, ",") != "reviewed-head,reviewed-head" {
		t.Fatalf("unexpected state after retry: runs=%+v heads=%v err=%v", runs, attemptedHeads, err)
	}
	if len(events) != 1 || !strings.Contains(events[0], "status=merged reason=merged duration_ms=120000") {
		t.Fatalf("expected one merge event with the full duration after restart, got %v", events)
	}
}

func TestThrottledIssueAdmissionReconcilesOriginalActivation(t *testing.T) {
	t.Parallel()
	w := definition("pending")
	w.IncludeBacklog = true
	e, items, _ := testEngine(t, w)
	*items = []Item{{Number: 17, Kind: "issue", Repository: w.Repository, Open: true, Labels: []string{"ready"}}}
	attempts := 0
	e.Submit = func(context.Context, Run) (string, error) { attempts++; return "", ErrIssueAdmissionPending }
	if err := e.Tick(t.Context()); !errors.Is(err, ErrIssueAdmissionPending) {
		t.Fatal(err)
	}
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Status != "pending_issue" {
		t.Fatal(runs, err)
	}
	id := runs[0].ID
	tick(t, e)
	e.Reconcile = func(event string) string {
		if event != id {
			t.Fatal(event)
		}
		return "recovered-worker"
	}
	tick(t, e)
	runs, err = e.Runs()
	if err != nil || len(runs) != 1 || runs[0].RequestID != "recovered-worker" || runs[0].Status != "running" || attempts != 1 {
		t.Fatal(runs, attempts, err)
	}
}
