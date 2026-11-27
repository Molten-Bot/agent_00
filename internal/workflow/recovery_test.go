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
	"github.com/Molten-Bot/agent_00/internal/githubutil"
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
	pr := Item{Open: true, HeadSHA: "abc", ChecksPassed: true, HumanApproved: true}
	e.InspectPR = func(context.Context, string, string) (Item, error) { return pr, nil }
	e.Merge = func(context.Context, Run) error {
		t.Fatal("legacy review flag authorized merge eligibility")
		return nil
	}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Status != awaitingReview || runs[0].MergeHead != "" {
		t.Fatalf("legacy run did not wait for a receipt: %+v, %v", runs, err)
	}
	pr.Open = false
	tick(t, e)
	runs, err = e.Runs()
	if err != nil || len(runs) != 0 {
		t.Fatalf("closed pull request run was not retired: %+v, %v", runs, err)
	}
}

func TestPendingReviewRetainsActivationWithoutUsingReviewEvidence(t *testing.T) {
	t.Parallel()
	w := definition("pending-review")
	w.ReviewCycles, w.Merge.Enabled = 1, true
	e, _, _ := testEngine(t, w)
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	item := Item{Repository: w.Repository, Kind: "issue", Number: 1}
	prURL := "https://github.com/acme/repo/pull/12"
	state.Runs = []Run{{ID: "pending", Workflow: w, Item: item, Status: "running", Outcome: &Outcome{
		Status: OutcomeReviewPending, PRURL: prURL, Reason: "Waiting for the reviewed head",
		ReviewReceipt: fixtureReviewReceipt(w, prURL),
	}}}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		t.Fatal("pending review reached merge eligibility")
		return Item{}, nil
	}
	tick(t, e)
	tick(t, e)
	state, err = e.load()
	if err != nil {
		t.Fatal(err)
	}
	run := state.run("pending")
	if run == nil || run.Status != awaitingReview || run.MergeHead != "" || state.busy(item) {
		t.Fatalf("pending review did not retain a retryable activation: %+v", run)
	}
}

func TestPendingChecksReserveActivationUntilCurrentHeadIsReviewed(t *testing.T) {
	t.Parallel()
	w := definition("pending-checks")
	w.ReviewCycles, w.Merge.Enabled = 1, true
	e, _, submitted := testEngine(t, w)
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	item := Item{Repository: w.Repository, Kind: "issue", Number: 1}
	prURL := "https://github.com/acme/repo/pull/12"
	old := fixtureReviewReceipt(w, prURL)
	state.Runs = []Run{{ID: "activation", RequestID: "task", Workflow: w, Item: item, Status: "running", MergeHead: old.Head}}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}
	if err := e.Complete("activation", Outcome{Status: OutcomeChecksPending, PRURL: prURL, Reason: "Waiting for GitHub check verification"}); err != nil {
		t.Fatal(err)
	}
	inspections, merges := 0, 0
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		inspections++
		return Item{Open: true, ChecksPassed: true, HumanApproved: true, HeadSHA: "new-head"}, nil
	}
	e.Merge = func(_ context.Context, run Run) error {
		merges++
		if run.ID != "activation" || run.RequestID != "task" || run.MergeHead != "new-head" {
			t.Fatalf("merge changed activation identity or used stale checks: %+v", run)
		}
		return nil
	}
	tick(t, e)
	tick(t, e)
	state, err = e.load()
	if err != nil {
		t.Fatal(err)
	}
	run := state.run("activation")
	if run == nil || run.Status != OutcomeChecksPending || run.MergeHead != "" || !state.busy(item) || inspections != 0 || *submitted != 0 {
		t.Fatalf("check wait lost activation or allowed polling or dispatch: %+v", run)
	}
	if err := e.Complete("activation", Outcome{Status: "succeeded", PRURL: prURL, ReviewReceipt: old}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if merges != 0 {
		t.Fatal("review on the earlier head authorized merge")
	}
	fresh := fixtureReviewReceipt(w, prURL)
	fresh.Head = "new-head"
	if err := e.Complete("activation", Outcome{Status: "succeeded", PRURL: prURL, ReviewReceipt: fresh}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if merges != 1 || *submitted != 0 {
		t.Fatalf("same activation did not recover: merges=%d submissions=%d", merges, *submitted)
	}
}

func TestPendingReviewRecoversFailedActivation(t *testing.T) {
	t.Parallel()
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("outcome_already_recorded=%t", recorded), func(t *testing.T) {
			w := definition("failed-review")
			w.ReviewCycles, w.Merge.Enabled = 1, true
			e, _, _ := testEngine(t, w)
			state, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			item := Item{Repository: w.Repository, Kind: "issue", Number: 1}
			prURL := "https://github.com/acme/repo/pull/12"
			outcome := Outcome{Status: OutcomeReviewPending, PRURL: prURL, Reason: "Waiting for the reviewed head",
				ReviewReceipt: fixtureReviewReceipt(w, prURL)}
			run := Run{ID: "failed", Workflow: w, Item: item, Status: "failed", MergeHead: "old-head"}
			if recorded {
				run.Outcome = &outcome
			}
			state.Runs = []Run{run}
			if err := writeAtomic(e.StatePath, state); err != nil {
				t.Fatal(err)
			}
			if !recorded {
				if err := e.Complete(run.ID, outcome); err != nil {
					t.Fatal(err)
				}
			}
			e.InspectPR = func(context.Context, string, string) (Item, error) {
				t.Fatal("pending review reached merge eligibility")
				return Item{}, nil
			}
			tick(t, e)
			tick(t, e)
			state, err = e.load()
			if err != nil {
				t.Fatal(err)
			}
			got := state.run(run.ID)
			if got == nil || got.Status != awaitingReview || got.PRURL != prURL || got.Reason != outcome.Reason ||
				got.MergeHead != "" || got.Outcome == nil || got.Outcome.Status != OutcomeReviewPending || state.busy(item) {
				t.Fatalf("failed activation did not retain pending review: %+v", got)
			}
		})
	}
}

func TestIssueRecoveryReplacesPendingReviewBeforeTick(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"running", "failed", awaitingReview} {
		t.Run(status, func(t *testing.T) {
			w := definition("pending-review-retry")
			e, _, _ := testEngine(t, w)
			item := Item{Repository: w.Repository, Kind: "issue", Number: 1}
			state, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			state.Runs = []Run{{ID: "original", Status: status, Workflow: w, Item: item,
				Outcome: &Outcome{Status: OutcomeReviewPending, PRURL: "https://github.com/acme/repo/pull/12"},
			}}
			if err := e.save(state); err != nil {
				t.Fatal(err)
			}
			if err := e.PrepareIssueRecovery(w.ID, w.ExecutionPolicyHash(), "retry", "original", item, nil); err != nil {
				t.Fatal(err)
			}
			runs, err := e.Runs()
			if err != nil || len(runs) != 1 || runs[0].ID != "retry" || runs[0].RetryOf != "original" || runs[0].Status != "pending_issue" {
				t.Fatalf("recovery did not replace the pending activation: %+v, %v", runs, err)
			}
		})
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
	e.Logf = func(format string, args ...any) { events = append(events, fmt.Sprintf(format, args...)) }
	e.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
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
	if err != nil || len(runs) != 0 || len(events) != 1 || !strings.HasSuffix(events[0], "status=succeeded reason=succeeded duration_ms=unknown") {
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
	var lastLog string
	e.Logf = func(format string, args ...any) { lastLog = fmt.Sprintf(format, args...) }
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
	if merges != 1 || !strings.Contains(lastLog, " status=waiting ") {
		t.Fatalf("after timeout merges=%d log=%s", merges, lastLog)
	}
	tick(t, e)
	runs, err := e.Runs()
	if merges != 1 || !strings.Contains(lastLog, " status=merged ") || err != nil || len(runs) != 0 {
		t.Fatalf("after reconciliation merges=%d log=%s runs=%+v err=%v", merges, lastLog, runs, err)
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

func TestWorkflowReviewCooldownRetainsActivationAndRechecksHeadAfterRestart(t *testing.T) {
	t.Parallel()
	for _, headAfterReset := range []string{"abc", "new-head"} {
		t.Run(headAfterReset, func(t *testing.T) {
			t.Parallel()
			w := definition("quota-review")
			w.Task.Kind = "review"
			w.Trigger = Trigger{Event: "pr.opened"}
			e, _, submissions := testEngine(t, w)
			prURL := "https://github.com/acme/repo/pull/42"
			now := time.Now()
			reset := now.Add(time.Minute)
			e.Now = func() time.Time { return now }
			receipt := fixtureReviewReceipt(w, prURL)
			state, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			state.Runs = []Run{{ID: "activation", RequestID: "task", Workflow: w,
				Item: Item{Repository: w.Repository, Kind: "pr", Number: 42}, Status: awaitingReview, PRURL: prURL,
				Outcome: &Outcome{Status: "succeeded", ReviewReceipt: receipt, PendingReviewGate: &githubutil.PendingReviewGate{MarkReady: true}}}}
			if err := e.save(state); err != nil {
				t.Fatal(err)
			}
			head := receipt.Head
			reads, writes := 0, 0
			e.InspectPR = func(context.Context, string, string) (Item, error) {
				reads++
				return Item{Open: true, Draft: true, HeadSHA: head}, nil
			}
			e.FinishReview = func(_ context.Context, _ githubutil.ReviewReceipt, gate *githubutil.PendingReviewGate) error {
				writes++
				if writes == 1 {
					gate.ThreadsReconciled = true
					return &githubutil.RateLimitError{RetryAt: reset}
				}
				if !gate.ThreadsReconciled {
					t.Fatal("lost review writeback checkpoint")
				}
				return nil
			}
			tick(t, e)
			restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed,
				Now: e.Now, Fetch: e.Fetch, Submit: e.Submit, Status: e.Status, InspectPR: e.InspectPR, FinishReview: e.FinishReview}
			tick(t, restarted)
			pending, err := restarted.load()
			if err != nil || len(pending.Runs) != 1 {
				t.Fatalf("workflow activation lost during cooldown: %+v %v", pending, err)
			}
			waiting := pending.Runs[0]
			if reads != 1 || writes != 1 || waiting.ID != "activation" || waiting.RequestID != "task" ||
				!waiting.GitHubRetryAt.Equal(reset) || waiting.Outcome.ReviewReceipt.Head != receipt.Head {
				t.Fatalf("cooldown changed workflow or read early: %+v reads=%d writes=%d", waiting, reads, writes)
			}
			now = reset
			head = headAfterReset
			tick(t, restarted)
			state, err = restarted.load()
			if err != nil || reads != 2 || *submissions != 0 {
				t.Fatalf("resume did not recheck the same activation: %+v reads=%d submitted=%d err=%v", state, reads, *submissions, err)
			}
			if head == receipt.Head {
				if writes != 2 || len(state.Runs) != 0 {
					t.Fatalf("current head did not finish: %+v writes=%d", state.Runs, writes)
				}
			} else if writes != 1 || len(state.Runs) != 1 || state.Runs[0].ID != waiting.ID ||
				state.Runs[0].Status != awaitingReview || !strings.Contains(state.Runs[0].Reason, "Head changed") {
				t.Fatalf("stale review was reused: %+v writes=%d", state.Runs, writes)
			}
		})
	}
}
