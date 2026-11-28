package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/tasktiming"
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
	pr := Item{Open: true, HeadSHA: "abc", ChecksPassed: true}
	e.InspectPRState = func(context.Context, string, string) (Item, error) { return pr, nil }
	e.Merge = func(context.Context, Run) error {
		t.Fatal("pending review authorized workflow merge")
		return nil
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

	pr = Item{Merged: true}
	var transitions []string
	e.Logf = func(format string, args ...any) { transitions = append(transitions, fmt.Sprintf(format, args...)) }
	tick(t, e)
	tick(t, e)
	if runs, err := e.Runs(); err != nil || len(runs) != 0 {
		t.Fatalf("external merge did not retire pending review: %+v, %v", runs, err)
	}
	if len(transitions) != 1 || !strings.Contains(transitions[0], "event=transition") ||
		!strings.Contains(transitions[0], "status=merged reason=merge_unconfirmed") {
		t.Fatalf("external merge did not emit one successful transition: %v", transitions)
	}
}

func TestObservedIssueMergeRetainsActivationUntilReviewEvidenceIsSaved(t *testing.T) {
	t.Parallel()
	w := definition("observed-issue-merge")
	w.Merge.Enabled = true
	e, _, _ := testEngine(t, w)
	state, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	const reason = "Waiting for unresolved review threads"
	run := Run{ID: "activation", RequestID: "task", Workflow: w, Status: awaitingReview, Reason: reason,
		PRURL: "https://github.com/acme/repo/pull/12", Item: Item{Repository: w.Repository, Kind: "issue", Number: 1}}
	state.Runs = []Run{run}
	if err := e.save(state); err != nil {
		t.Fatal(err)
	}
	mergedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		return Item{Merged: true, MergedAt: mergedAt}, nil
	}
	writeError := errors.New("issue history unavailable")
	e.RecordIssueMerge = func(previous Run, pr Item, mergedByApp bool) error {
		if mergedByApp || previous.Reason != reason || previous.RequestID != run.RequestID || pr.URL != run.PRURL || !pr.MergedAt.Equal(mergedAt) {
			t.Fatalf("incorrect merge or evidence passed to issue history: %+v, %+v", previous, pr)
		}
		return writeError
	}
	if err := e.Tick(t.Context()); !errors.Is(err, writeError) {
		t.Fatalf("tick with unavailable issue history: %v", err)
	}
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Reason != reason {
		t.Fatalf("activation lost evidence before it was saved: %+v, %v", runs, err)
	}
	writeError = nil
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed,
		Fetch: e.Fetch, RecordIssueMerge: e.RecordIssueMerge,
		InspectPR: func(context.Context, string, string) (Item, error) {
			t.Fatal("looked up a previously observed merge after restart")
			return Item{}, nil
		}}
	tick(t, restarted)
	if runs, err = restarted.Runs(); err != nil || len(runs) != 0 {
		t.Fatalf("merged activation was not pruned after saving evidence: %+v, %v", runs, err)
	}
}

func TestIssueMergeReceiptRetriesAfterWorkflowRestart(t *testing.T) {
	t.Parallel()
	for _, byApp := range []bool{false, true} {
		name := "worker reported merge"
		if byApp {
			name = "app merge"
		}
		t.Run(name, func(t *testing.T) {
			w := definition("issue-merge")
			w.Merge.Enabled, w.Merge.RequireApproval = true, false
			e, _, _ := testEngine(t, w)
			state, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			const prURL = "https://github.com/acme/repo/pull/12"
			run := Run{ID: "activation", RequestID: "task", Workflow: w, Status: "waiting", PRURL: prURL,
				Item: Item{Repository: w.Repository, Kind: "issue", Number: 1}}
			if !byApp {
				run.Status, run.PRURL = "running", ""
				run.Outcome = &Outcome{Status: "failed", ReviewInterrupted: true, PRURL: prURL, Reason: "review: merged before verification"}
			}
			state.Runs = []Run{run}
			if err := e.save(state); err != nil {
				t.Fatal(err)
			}
			merges := 0
			mergedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
			e.InspectPR = func(context.Context, string, string) (Item, error) {
				if !byApp {
					t.Fatal("worker already observed the merge")
				}
				if merges == 0 {
					return Item{Open: true, HeadSHA: "head", ChecksPassed: true}, nil
				}
				restarted := &Engine{StatePath: e.StatePath}
				runs, err := restarted.Runs()
				if err != nil || len(runs) != 1 || !runs[0].PRMergeConfirmed() || runs[0].AppMerge == nil {
					t.Fatalf("app attribution missing before timestamp lookup: runs=%+v err=%v", runs, err)
				}
				return Item{Merged: true, MergedAt: mergedAt}, nil
			}
			e.Merge = func(context.Context, Run) error {
				merges++
				return nil
			}
			writeErr := errors.New("issue receipt unavailable")
			writes := 0
			e.RecordIssueMerge = func(previous Run, pr Item, mergedByApp bool) error {
				writes++
				if previous.ID != run.ID || pr.URL != prURL || !pr.Merged || mergedByApp != byApp ||
					(byApp && !pr.MergedAt.Equal(mergedAt)) || (!byApp && previous.Reason != run.Outcome.Reason) {
					t.Fatalf("incorrect merge receipt: %+v, %+v, by app %t", previous, pr, mergedByApp)
				}
				if writes == 1 {
					return writeErr
				}
				return nil
			}
			if err := e.Tick(t.Context()); !errors.Is(err, writeErr) {
				t.Fatalf("first tick error = %v, want %v", err, writeErr)
			}
			restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed,
				Fetch: e.Fetch, RecordIssueMerge: e.RecordIssueMerge}
			tick(t, restarted)
			runs, err := restarted.Runs()
			if err != nil || len(runs) != 0 || writes != 2 || (merges == 1) != byApp || merges > 1 {
				t.Fatalf("merge receipt retry: runs %+v, err %v, writes %d, app merges %d", runs, err, writes, merges)
			}
		})
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

func TestPausedWorkflowReconcilesExternalMergeWithoutWriteback(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, status string
		outcome      *Outcome
	}{
		{name: "checks pending", status: OutcomeChecksPending},
		{name: "review pending", status: awaitingReview, outcome: &Outcome{Status: OutcomeReviewPending}},
		{name: "writeback pending", status: awaitingReview, outcome: &Outcome{Status: "succeeded", PendingReviewGate: &githubutil.PendingReviewGate{MarkReady: true}}},
		{name: "merge pending", status: "waiting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			w := definition("paused")
			w.Merge.Enabled = true
			paused := w
			paused.Enabled = false
			e, _, submissions := testEngine(t, paused)
			const prURL = "https://github.com/acme/repo/pull/42"
			item := Item{Kind: "issue", Repository: w.Repository, Number: 1}
			run := Run{ID: "activation", RequestID: "task", Workflow: w, Item: item, PRURL: prURL, Status: test.status, Outcome: test.outcome}
			if run.Outcome != nil && run.Outcome.PendingReviewGate != nil {
				run.Outcome.PRURL = prURL
				run.Outcome.ReviewReceipt = fixtureReviewReceipt(w, prURL)
			}
			state, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			state.Runs = []Run{run}
			if err := e.save(state); err != nil {
				t.Fatal(err)
			}
			e.InspectPR = func(context.Context, string, string) (Item, error) {
				t.Fatal("paused workflow ran eligibility inspection")
				return Item{}, nil
			}
			e.FinishReview = func(context.Context, githubutil.ReviewReceipt, *githubutil.PendingReviewGate) error {
				t.Fatal("paused workflow ran writeback")
				return nil
			}
			e.Merge = func(context.Context, Run) error {
				t.Fatal("paused workflow requested merge")
				return nil
			}
			mergedAt := time.Now().UTC()
			pr := Item{Open: true}
			e.InspectPRState = func(_ context.Context, repo, url string) (Item, error) {
				if repo != w.Repository || url != prURL {
					t.Fatalf("unexpected inspection of %s %s", repo, url)
				}
				return pr, nil
			}
			recorded := 0
			e.RecordMerge = func(got Run, at time.Time) error {
				recorded++
				if got.ID != run.ID || got.RequestID != run.RequestID || got.Status != "merged" ||
					got.Reason != externalMergeReason || !at.Equal(mergedAt) {
					t.Fatalf("unexpected merge receipt: %+v at %v", got, at)
				}
				return nil
			}
			tick(t, e)
			if runs, err := e.Runs(); err != nil || len(runs) != 1 || runs[0].Status != test.status || recorded != 0 {
				t.Fatalf("open PR changed paused activation: %+v, %v, receipts=%d", runs, err, recorded)
			}
			pr = Item{Merged: true, MergedAt: mergedAt}
			tick(t, e)
			tick(t, e)
			if runs, err := e.Runs(); err != nil || len(runs) != 0 || recorded != 1 || *submissions != 0 {
				t.Fatalf("confirmed merge did not retire once: %+v, %v, receipts=%d, submissions=%d", runs, err, recorded, *submissions)
			}
		})
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
			e.InspectPRState = func(context.Context, string, string) (Item, error) {
				return Item{Open: true}, nil
			}
			e.Merge = func(context.Context, Run) error {
				t.Fatal("pending review authorized workflow merge")
				return nil
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
			now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
			e.Now = func() time.Time { return now }
			var log strings.Builder
			e.TimingLog = func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }
			wait := tasktiming.Start("original", "wait", tasktiming.WorkflowWait, now.Add(-time.Second))
			item := Item{Repository: w.Repository, Kind: "issue", Number: 1}
			state, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			state.Runs = []Run{{ID: "original", RequestID: "task", Status: status, Workflow: w, Item: item, TimingWait: &wait,
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
			spans, err := tasktiming.Read(t.Context(), strings.NewReader(log.String()), "task")
			if err != nil || len(spans) != 1 || spans[0].ID != wait.ID || spans[0].EndedAt == nil || !spans[0].EndedAt.Equal(now) {
				t.Fatalf("prior review wait did not close on recovery: %+v, %v", spans, err)
			}
		})
	}
}

func TestPendingReviewUsesGitHubMergeTimeAfterRestart(t *testing.T) {
	t.Parallel()

	w := definition("pending-review-merge")
	e, _, _ := testEngine(t, w)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergedAt := start.Add(2 * time.Minute)
	now := start.Add(time.Minute)
	e.Now = func() time.Time { return now }
	const prURL = "https://github.com/acme/repo/pull/12"
	if err := e.transaction(func(s *State) error {
		s.Runs = []Run{{ID: "activation", RequestID: "task", Status: awaitingReview,
			Workflow: w, Item: Item{Repository: w.Repository, Kind: "issue", Number: 1},
			StartedAt: &start, PRURL: prURL, Outcome: &Outcome{Status: OutcomeReviewPending, PRURL: prURL}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	state := `{"state":"OPEN","mergedAt":null,"autoMergeRequest":null,"mergeQueueEntry":null}`
	calls := 0
	gh := GitHub{Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		calls++
		want := []string{"api", "graphql", "-f",
			"query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state mergedAt autoMergeRequest{enabledAt} mergeQueueEntry{id}}}}",
			"-f", "owner=acme", "-f", "name=repo", "-F", "number=12", "--jq", ".data.repository.pullRequest"}
		if cmd.Name != "gh" || !slices.Equal(cmd.Args, want) {
			t.Fatalf("unexpected GitHub command: %s %v", cmd.Name, cmd.Args)
		}
		return execx.Result{Stdout: state}, nil
	})}
	e.InspectPR, e.InspectPRState = gh.Inspect, gh.InspectState
	tick(t, e)
	if runs, err := e.Runs(); err != nil || len(runs) != 1 || runs[0].Status != awaitingReview || calls != 1 {
		t.Fatalf("open pull request did not stay in review after one lookup: %+v, err=%v, calls=%d", runs, err, calls)
	}

	now = start.Add(24 * time.Hour)
	state = fmt.Sprintf(`{"state":"MERGED","mergedAt":%q}`, mergedAt.Format(time.RFC3339))
	var transitions []string
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Now: e.Now,
		Fetch: e.Fetch, Status: e.Status, InspectPR: gh.Inspect, InspectPRState: gh.InspectState,
		Logf: func(format string, args ...any) { transitions = append(transitions, fmt.Sprintf(format, args...)) }}
	tick(t, restarted)
	tick(t, restarted)
	if runs, err := restarted.Runs(); err != nil || len(runs) != 0 || calls != 2 {
		t.Fatalf("merged pull request did not retire after one more lookup: %+v, err=%v, calls=%d", runs, err, calls)
	}
	if len(transitions) != 1 || !strings.Contains(transitions[0], "merged_at="+mergedAt.Format(time.RFC3339)) ||
		!strings.HasSuffix(transitions[0], "status=merged reason=merge_unconfirmed duration_ms=120000") {
		t.Fatalf("expected two minutes of workflow time and the GitHub merge timestamp: %v", transitions)
	}
}

func TestFailedPRReviewWatchesForMergeAfterRestart(t *testing.T) {
	t.Parallel()
	for _, merged := range []bool{true, false} {
		t.Run(fmt.Sprintf("merged=%t", merged), func(t *testing.T) {
			t.Parallel()
			w := definition("failed-pr-review")
			w.Trigger = Trigger{Event: "pr.opened"}
			w.Task.Kind = "review"
			w.Merge.Enabled = true
			e, _, _ := testEngine(t, w)
			const prURL = "https://github.com/acme/repo/pull/42"
			item := Item{Kind: "pr", Repository: w.Repository, Number: 42, URL: prURL}
			if err := e.transaction(func(s *State) error {
				s.Runs = []Run{{ID: "activation", RetryOf: "original", RequestID: "review-task", Workflow: w, Item: item, Status: "running"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.Complete("activation", Outcome{Status: "failed", Reason: "invalid structured review output"}); err != nil {
				t.Fatal(err)
			}
			e.InspectPRState = func(context.Context, string, string) (Item, error) { return Item{Open: true}, nil }
			tick(t, e)
			state, err := e.load()
			if err != nil || len(state.Runs) != 1 || state.Runs[0].Status != "failed" || state.busy(item) {
				t.Fatalf("open PR failed review should stay failed and release its reservation: %+v, %v", state.Runs, err)
			}
			submitErr := errors.New("retry submission failed")
			if _, err := e.DispatchRetry(t.Context(), w.ID, w.ExecutionPolicyHash(), "original", item, nil, func(Run) (string, error) {
				return "", submitErr
			}); !errors.Is(err, submitErr) {
				t.Fatalf("failed review watch blocked explicit retry: %v", err)
			}

			mergedAt := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)
			inspections, recorded := 0, 0
			restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch,
				InspectPRState: func(_ context.Context, repo, url string) (Item, error) {
					inspections++
					if repo != w.Repository || url != prURL {
						t.Fatalf("inspected %s %s", repo, url)
					}
					if merged {
						return Item{Merged: true, MergedAt: mergedAt}, nil
					}
					return Item{}, nil
				},
				RecordMerge: func(run Run, got time.Time) error {
					recorded++
					if run.RequestID != "review-task" || run.Reason != externalMergeReason || !got.Equal(mergedAt) {
						t.Fatalf("unexpected merge receipt: %+v, %v", run, got)
					}
					return nil
				},
			}
			tick(t, restarted)
			tick(t, restarted)
			runs, err := restarted.Runs()
			if err != nil || len(runs) != 0 || inspections != 1 || (recorded == 1) != merged {
				t.Fatalf("terminal PR left a watch or incorrect receipt: %+v, %v, inspections=%d, receipts=%d", runs, err, inspections, recorded)
			}
		})
	}
}

func TestAcceptedMergeRequestIsReconciledWithoutResubmission(t *testing.T) {
	t.Parallel()
	for _, mergeErr := range []error{nil, errMergePending} {
		t.Run(fmt.Sprint(mergeErr), func(t *testing.T) {
			t.Parallel()
			w := definition("queued-merge")
			w.Merge.Enabled, w.Merge.RequireApproval = true, false
			e, _, _ := testEngine(t, w)
			const prURL = "https://github.com/acme/repo/pull/42"
			if err := e.transaction(func(s *State) error {
				s.Runs = []Run{{ID: "activation", Workflow: w, Item: Item{Repository: w.Repository}, Status: "waiting", PRURL: prURL}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			e.InspectPR = func(context.Context, string, string) (Item, error) {
				return Item{Open: true, ChecksPassed: true, HeadSHA: "abc"}, nil
			}
			open := func(context.Context, string, string) (Item, error) { return Item{Open: true}, nil }
			e.InspectPRState = open
			merges := 0
			e.Merge = func(context.Context, Run) error {
				merges++
				return mergeErr
			}
			tick(t, e)
			runs, err := e.Runs()
			if err != nil || len(runs) != 1 || merges != 1 {
				t.Fatalf("merge did not leave one pending run: %+v, %v, submissions=%d", runs, err, merges)
			}

			mergedAt := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)
			recorded := 0
			restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch,
				InspectPRState: open, InspectPR: e.InspectPR, Merge: e.Merge,
				RecordMerge: func(run Run, got time.Time) error {
					recorded++
					if run.Reason != "Pull request merged" || !got.Equal(mergedAt) {
						t.Fatalf("accepted app merge misclassified: %+v, %v", run, got)
					}
					return nil
				},
			}
			tick(t, restarted)
			restarted.InspectPRState = func(context.Context, string, string) (Item, error) {
				return Item{Merged: true, MergedAt: mergedAt}, nil
			}
			restarted.InspectPR = restarted.InspectPRState
			tick(t, restarted)
			tick(t, restarted)
			if runs, err := restarted.Runs(); err != nil || len(runs) != 0 || merges != 1 || recorded != 1 {
				t.Fatalf("queue did not settle once: %+v, %v, submissions=%d, receipts=%d", runs, err, merges, recorded)
			}
		})
	}
}

func TestCanceledMergeRequestRechecksEligibility(t *testing.T) {
	t.Parallel()
	w := definition("canceled-merge")
	w.Merge.Enabled, w.Merge.RequireApproval = true, false
	e, _, _ := testEngine(t, w)
	const prURL = "https://github.com/acme/repo/pull/42"
	if err := e.transaction(func(s *State) error {
		s.Runs = []Run{{ID: "activation", Workflow: w, Item: Item{Repository: w.Repository},
			Status: "waiting", PRURL: prURL, MergeHead: "abc", MergeRequested: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := `{"state":"OPEN","autoMergeRequest":null,"mergeQueueEntry":{"id":"entry"}}`
	gh := GitHub{Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		args := strings.Join(cmd.Args, " ")
		if !strings.HasPrefix(args, "api graphql ") || !strings.Contains(args, "autoMergeRequest{enabledAt} mergeQueueEntry{id}") ||
			!slices.Contains(cmd.Args, "number=42") {
			t.Fatalf("unexpected merge status query: %v", cmd.Args)
		}
		return execx.Result{Stdout: state}, nil
	})}
	e.InspectPRState = gh.InspectState
	checksPassed := false
	inspections, merges := 0, 0
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		inspections++
		return Item{Open: true, HeadSHA: "abc", ChecksPassed: checksPassed}, nil
	}
	e.Merge = func(context.Context, Run) error {
		merges++
		return errMergePending
	}
	checkRequested := func(want bool) {
		t.Helper()
		runs, err := e.Runs()
		if err != nil || len(runs) != 1 || runs[0].MergeRequested != want {
			t.Fatalf("merge requested should be %t: %+v, %v", want, runs, err)
		}
	}

	tick(t, e)
	checkRequested(true)
	state = `{"state":"OPEN","autoMergeRequest":{"enabledAt":"2026-01-01T00:00:00Z"},"mergeQueueEntry":null}`
	tick(t, e)
	checkRequested(true)
	state = `{"state":"OPEN","autoMergeRequest":null,"mergeQueueEntry":null}`
	tick(t, e)
	checkRequested(false)
	if inspections != 0 || merges != 0 {
		t.Fatalf("inspected or resubmitted while polling: inspections=%d merges=%d", inspections, merges)
	}
	tick(t, e)
	if inspections != 1 || merges != 0 {
		t.Fatalf("resubmitted before checks passed: inspections=%d merges=%d", inspections, merges)
	}
	checksPassed = true
	tick(t, e)
	checkRequested(true)
	if inspections != 2 || merges != 1 {
		t.Fatalf("did not resubmit through normal eligibility: inspections=%d merges=%d", inspections, merges)
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
			workerFinished := now.Add(-time.Minute)
			reset := now.Add(time.Minute)
			e.Now = func() time.Time { return now }
			receipt := fixtureReviewReceipt(w, prURL)
			state, err := e.load()
			if err != nil {
				t.Fatal(err)
			}
			state.Runs = []Run{{ID: "activation", RequestID: "task", Workflow: w,
				Item: Item{Repository: w.Repository, Kind: "pr", Number: 42}, Status: awaitingReview, PRURL: prURL,
				StartedAt: &workerFinished, Outcome: &Outcome{Status: "succeeded", CompletedAt: workerFinished,
					ReviewReceipt: receipt, PendingReviewGate: &githubutil.PendingReviewGate{MarkReady: true}}}}
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
				now = now.Add(3 * time.Second)
				return nil
			}
			tick(t, e)
			var transitions []string
			restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Enabled: e.Enabled, Allowed: e.Allowed,
				Now: e.Now, Fetch: e.Fetch, Submit: e.Submit, Status: e.Status, InspectPR: e.InspectPR, FinishReview: e.FinishReview,
				Logf: func(format string, args ...any) { transitions = append(transitions, fmt.Sprintf(format, args...)) }}
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
				if len(transitions) != 1 || !strings.HasSuffix(transitions[0], "status=succeeded reason=succeeded duration_ms=123000") {
					t.Fatalf("completion timing omitted deferred writeback: %v", transitions)
				}
			} else if writes != 1 || len(state.Runs) != 1 || state.Runs[0].ID != waiting.ID ||
				state.Runs[0].Status != awaitingReview || !strings.Contains(state.Runs[0].Reason, "Head changed") {
				t.Fatalf("stale review was reused: %+v writes=%d", state.Runs, writes)
			}
		})
	}
}

// A retained stop takes precedence over GitHub cooldown and PR delivery work.
func TestStoppedIssueReleasesWaitingWorkflow(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"pending_issue", OutcomeChecksPending, "waiting", awaitingReview} {
		t.Run(status, func(t *testing.T) {
			w := definition("issue-stop")
			e, _, _ := testEngine(t, w)
			item := Item{Repository: w.Repository, Kind: "issue", Number: 42}
			if err := e.transaction(func(s *State) error {
				s.Runs = append(s.Runs, Run{
					ID: "activation", Item: item, Workflow: w, Status: status,
					GitHubRetryAt: time.Now().Add(time.Hour),
				})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stopped := false
			e.IssueTaskStopped = func(id string) bool { return id == "activation" && stopped }
			if err := e.StopIssueTask(func() error { stopped = true; return nil }); err != nil {
				t.Fatal(err)
			}
			tick(t, e)
			if runs, err := e.Runs(); err != nil || len(runs) != 0 {
				t.Fatalf("stopped workflow still reserves the issue: %+v (error %v)", runs, err)
			}
		})
	}
}
