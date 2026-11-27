package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestCheckPollDelayLearnsAndTightensNearCompletion(t *testing.T) {
	t.Parallel()
	var history CheckTimings
	const key = "example/repo/checks"
	if got := checkPollDelay(history.average(key), 0); got != 10*time.Second {
		t.Fatalf("unknown check interval = %v, want 10s", got)
	}
	history.record(key, 110*time.Second)
	history.record(key, 150*time.Second)
	expected := history.average(key)
	if expected != 120*time.Second {
		t.Fatalf("learned check duration = %v, want 2m", expected)
	}
	for _, tc := range []struct{ elapsed, want time.Duration }{
		{0, 45 * time.Second},
		{110 * time.Second, 5 * time.Second},
		{150 * time.Second, 10 * time.Second},
	} {
		if got := checkPollDelay(expected, tc.elapsed); got != tc.want {
			t.Errorf("interval at %v = %v, want %v", tc.elapsed, got, tc.want)
		}
	}
	if history.average("different/repo/checks") != 0 {
		t.Fatal("another repository inherited the check duration")
	}
}

type pendingCheckExit struct{}

func (pendingCheckExit) Error() string { return "exit status 8" }
func (pendingCheckExit) ExitCode() int { return 8 }

func TestPRCheckWatchReportsChecksWithSchedulerReleased(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	cmd := prChecksTextCommand("/checkout", url, false)
	runner := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: cmd, res: execx.Result{Stdout: "Go\tpass\nBrowser tests\tpending\n"}, err: pendingCheckExit{}},
		{cmd: cmd, res: execx.Result{Stdout: "Go\tpass\nBrowser tests\tpass\n"}},
	}}
	h := New(runner)
	h.CheckTimings = &CheckTimings{}
	h.CheckTimings.record("example/repo/checks", 120*time.Second)
	released := false
	h.ReleaseWorkspace = func() { released = true }
	h.Sleep = func(_ context.Context, delay time.Duration) error {
		if !released || delay < 35*time.Second {
			t.Fatalf("pending checks: released = %t, delay = %v", released, delay)
		}
		return nil
	}
	var logs []string
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	if _, err := h.runPRChecksWatch(t.Context(), prChecksCommand("/checkout", url), newCheckPoll("checks", url)); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(logs, "\n")
	for _, want := range []string{
		`status=waiting action=wait_for_github_checks`, `pending_checks="Browser tests"`,
		`last_github_refresh="`, `action=github_checks_finished elapsed_ms=`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q from check progress", want)
		}
	}
}

func TestPublishedChecksRecordMergeBeforeReview(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	repo := repoWorkspace{Dir: "/checkout", PRURL: url, IssueClaimBranch: true}
	runner := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "current"}},
		{cmd: prStateViewCommand(repo.Dir, url), res: execx.Result{Stdout: `{"state":"OPEN","headRefOid":"current"}`}},
		{cmd: prChecksTextCommand(repo.Dir, url, false),
			res: execx.Result{Stdout: "CI\tpending\n"}, err: pendingCheckExit{}},
		{cmd: prChecksTextCommand(repo.Dir, url, false), res: execx.Result{Stdout: "CI\tpass\n"}},
		{cmd: prStateViewCommand(repo.Dir, url, "id"), res: execx.Result{Stdout: `{"state":"MERGED"}`}},
	}}
	h := New(runner)
	h.CheckTimings = &CheckTimings{}
	released := 0
	h.ReleaseWorkspace = func() { released++ }
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.AcquireWorkspace = func(context.Context) error {
		t.Fatal("merged PR reacquired workspace")
		return nil
	}
	runtime := agentruntime.Default()
	cfg := config.Config{}
	if code, stage, err := h.checkPublishedPullRequest(t.Context(), cfg, &repo, runtime, "", codexRunOptions{}, "", "", "", "", false); code != ExitSuccess || err != nil {
		t.Fatalf("published checks = (%d, %q, %v)", code, stage, err)
	}
	if !repo.PRAlreadyMerged || repo.ReviewInterrupted || released != 2 {
		t.Fatalf("merged = %t, interrupted = %t, releases = %d", repo.PRAlreadyMerged, repo.ReviewInterrupted, released)
	}
	if _, stage, err := h.finishPublishedPullRequest(t.Context(), cfg, &repo, runtime, "", codexRunOptions{}, "", "", "", "", false); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	if len(runner.exps) != 0 {
		t.Fatalf("unconsumed commands = %d", len(runner.exps))
	}
}

func TestResumedCheckWaitUsesOnlyGitHubUntilChecksSettle(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	const open = `{"state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current","statusCheckRollup":`
	const passing = `[{"name":"CI","conclusion":"SUCCESS"}]`
	cmd := prStateViewCommand("", url, "statusCheckRollup")
	protection := requiredStatusChecksCommand("", "example/repo", "main")
	rules := ghCommand("", "api", "--paginate", "repos/example/repo/rules/branches/main?per_page=100")
	dispatch := workflowDispatchRunsCommand("", "task")
	dispatch.Args = append(dispatch.Args, "--repo", "example/repo", "--commit", "current")
	for _, tc := range []struct {
		name, initial, final, classic, rules, run string
		merged                                    bool
	}{
		{name: "pending then passing", initial: `[{"name":"CI","status":"IN_PROGRESS"}]`, final: open + passing + "}", classic: `["CI"]`, rules: "[]"},
		{name: "empty then passing", initial: "[]", final: open + passing + "}", classic: `["CI"]`, rules: "[]", run: "[]"},
		{name: "optional pass with missing classic check", initial: `[{"name":"optional","conclusion":"SUCCESS"}]`,
			final: open + passing + "}", classic: `["CI"]`, rules: "[]"},
		{name: "optional pass with missing ruleset check", initial: `[{"name":"optional","conclusion":"SUCCESS"}]`,
			final: open + passing + "}", classic: "[]", rules: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"CI"}]}}]`},
		{name: "no required CI and no dispatch", initial: "[]", classic: "[]", rules: "[]", run: "[]"},
		{name: "completed dispatch without rollup and no required CI", initial: "[]", classic: "[]", rules: "[]",
			run: `[{"headSha":"current","status":"completed","conclusion":"success"}]`},
		{name: "completed successful dispatch cannot satisfy required checks", initial: "[]", final: open + passing + "}", classic: `["CI"]`, rules: "[]",
			run: `[{"headSha":"current","status":"completed","conclusion":"success"}]`},
		{name: "completed failed dispatch reaches remediation", initial: "[]", classic: `["CI"]`, rules: "[]",
			run: `[{"headSha":"current","status":"completed","conclusion":"failure"}]`},
		{name: "pending dispatch then merged", initial: `[{"name":"agent00 / review-policy","conclusion":"SUCCESS"}]`,
			final: `{"state":"MERGED"}`, merged: true, classic: "[]", rules: "[]",
			run: `[{"headSha":"current","status":"in_progress"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{t: t, exps: []expectedRun{
				{cmd: cmd, res: execx.Result{Stdout: open + tc.initial + "}"}},
			}}
			policy := []expectedRun{
				{cmd: protection, res: execx.Result{Stdout: `{"contexts":` + tc.classic + "}"}},
				{cmd: rules, res: execx.Result{Stdout: tc.rules}},
			}
			pending := strings.Contains(tc.initial, "IN_PROGRESS")
			if !pending {
				runner.exps = append(runner.exps, policy...)
			}
			if tc.run != "" {
				runner.exps = append(runner.exps, expectedRun{cmd: dispatch, res: execx.Result{Stdout: tc.run}})
			}
			if tc.final != "" {
				runner.exps = append(runner.exps, expectedRun{cmd: cmd, res: execx.Result{Stdout: tc.final}})
			}
			if pending {
				runner.exps = append(runner.exps, policy...)
			}
			h := New(runner)
			h.CheckTimings = &CheckTimings{}
			const estimate = 2 * time.Minute
			key := newCheckPoll("checks", url).key
			h.CheckTimings.record(key, estimate)
			h.PRChecksWatchTimeout = time.Minute
			h.Sleep = func(_ context.Context, delay time.Duration) error {
				if delay != 45*time.Second {
					t.Fatalf("delay = %v, want 45s", delay)
				}
				return nil
			}
			h.AcquireWorkspace = func(context.Context) error {
				t.Fatal("remote check wait tried to acquire workspace")
				return nil
			}
			if merged, err := h.waitForResumedChecks(t.Context(), url); merged != tc.merged || err != nil || len(runner.exps) != 0 {
				t.Fatalf("remote check wait = (%t, %v), want (%t, nil), with %d queries remaining", merged, err, tc.merged, len(runner.exps))
			}
			if got := h.CheckTimings.average(key); got != estimate {
				t.Fatalf("resumed wait changed build estimate to %v, want %v", got, estimate)
			}
		})
	}
}

func TestResumedCheckWaitDeadlineIncludesQueriesAndRetries(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	for _, tc := range []struct {
		name, rollup string
		fail, policy bool
	}{
		{name: "pending check", rollup: `[{"name":"CI","status":"IN_PROGRESS"}]`},
		{name: "no checks reported", rollup: `[]`, policy: true},
		{name: "transient query failure", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const timeout = 5 * time.Second
				start := time.Now()
				calls := 0
				h := New(issueBranchRunner(func(ctx context.Context, _ execx.Command) (execx.Result, error) {
					calls++
					deadline, ok := ctx.Deadline()
					if !ok || !deadline.Equal(start.Add(timeout)) {
						t.Fatalf("query deadline = %v, want %v", deadline, start.Add(timeout))
					}
					if tc.fail {
						if calls == 1 {
							return execx.Result{Stderr: "HTTP 503: Service Unavailable"}, errors.New("exit status 1")
						}
						<-ctx.Done()
						return execx.Result{}, ctx.Err()
					}
					if tc.policy && calls == 2 {
						<-ctx.Done()
						return execx.Result{}, ctx.Err()
					}
					time.Sleep(3 * time.Second)
					return execx.Result{Stdout: `{"state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current","statusCheckRollup":` + tc.rollup + `}`}, nil
				}))
				h.PRChecksWatchTimeout = timeout
				merged, err := h.waitForResumedChecks(t.Context(), url)
				if merged || !errors.Is(err, ErrChecksUnavailable) || time.Since(start) != timeout {
					t.Fatalf("resumed check wait = (%t, %v) after %v", merged, err, time.Since(start))
				}
				wantCalls := 1
				if tc.fail || tc.policy {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("queries = %d, want %d", calls, wantCalls)
				}
			})
		})
	}
}

func TestNoReportedCheckRetriesTightenWithoutReadmittingWorkspace(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const url = "https://github.com/example/repo/pull/42"
		repo := repoWorkspace{Dir: "/checkout", PRURL: url}
		noChecks := expectedRun{
			cmd: prChecksTextCommand(repo.Dir, url, false),
			res: execx.Result{Stderr: "no checks reported"},
			err: errors.New("exit status 1"),
		}
		runner := &fakeRunner{t: t, exps: []expectedRun{
			noChecks, noChecks,
			{cmd: noChecks.cmd, res: execx.Result{Stdout: "CI\tpending\n"}, err: pendingCheckExit{}},
			{cmd: noChecks.cmd, res: execx.Result{Stdout: "CI\tpass\n"}},
			{cmd: prMergeabilityCommand(repo.Dir, url), res: execx.Result{Stdout: `{"state":"OPEN","mergeable":"MERGEABLE"}`}},
		}}
		h := New(runner)
		h.IssueChecksPRURL = url
		h.CheckTimings = &CheckTimings{}
		h.CheckTimings.record("example/repo/checks", 90*time.Second)
		released := false
		h.ReleaseWorkspace = func() { released = true }
		h.AcquireWorkspace = func(context.Context) error {
			t.Fatal("remote-only completion attempted workspace admission")
			return nil
		}
		var delays []time.Duration
		h.Sleep = func(ctx context.Context, delay time.Duration) error {
			if !released {
				t.Fatal("workspace retained during no-check retry")
			}
			delays = append(delays, delay)
			return sleepWithContext(ctx, delay)
		}
		runtime := agentruntime.Default()
		cfg := config.Config{}
		if _, stage, err := h.checkPublishedPullRequest(t.Context(), cfg, &repo, runtime, "", codexRunOptions{}, "", "", "", "", false); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if _, stage, err := h.finishPublishedPullRequest(t.Context(), cfg, &repo, runtime, "", codexRunOptions{}, "", "", "", "", false); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if len(delays) != 3 || delays[0] != 45*time.Second || delays[1] != 22500*time.Millisecond || delays[2] != 11250*time.Millisecond {
			t.Fatalf("polling delays = %v, want [45s 22.5s 11.25s]", delays)
		}
		if got := h.CheckTimings.average("example/repo/checks"); got != 87187500*time.Microsecond {
			t.Fatalf("learned duration = %v, want 87.1875s including the no-check wait", got)
		}
		if len(runner.exps) != 0 {
			t.Fatalf("unconsumed commands = %d", len(runner.exps))
		}
	})
}

func TestPublishedResumeReconcilesEmptyRollupBeforeRetrying(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	repo := repoWorkspace{Dir: "/checkout", PRURL: url, BaseBranch: "main", Branch: "task", IssueClaimBranch: true}
	const state = `{"id":"PR_id","state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current"}`
	protection := expectedRun{cmd: requiredStatusChecksCommand(repo.Dir, "example/repo", "main"),
		res: execx.Result{Stdout: `{"contexts":[]}`}}
	rules := expectedRun{cmd: ghCommand(repo.Dir, "api", "--paginate", "repos/example/repo/rules/branches/main?per_page=100"),
		res: execx.Result{Stdout: "[]"}}
	dispatch := workflowDispatchRunsCommand(repo.Dir, repo.Branch)
	dispatch.Args = append(dispatch.Args, "--repo", "example/repo", "--commit", "current")
	runner := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "current"}},
		{cmd: prStateViewCommand(repo.Dir, url), res: execx.Result{Stdout: state}},
		{cmd: prChecksCommand(repo.Dir, url), res: execx.Result{Stderr: "no checks reported"}, err: errors.New("exit status 1")},
		protection, rules,
		{cmd: dispatch, res: execx.Result{Stdout: `[{"headSha":"current","status":"completed","conclusion":"success"}]`}},
		{cmd: prStateViewCommand(repo.Dir, url, "id"), res: execx.Result{Stdout: state}},
		protection, rules,
		{cmd: ghCommand(repo.Dir, "api", "graphql", "-f", "id=PR_id", "-f", "query="+prCheckRollupQuery),
			res: execx.Result{Stdout: checkRollupPageJSON(repo.Branch, "current", "current", "[]", false, "")}},
	}}
	h := New(runner)
	h.IssueChecksPRURL = url
	h.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("resumed task slept after dispatch completed with no required checks")
		return nil
	}
	h.AcquireWorkspace = func(context.Context) error {
		t.Fatal("check reconciliation tried to reacquire workspace")
		return nil
	}
	code, stage, err := h.checkPublishedPullRequest(t.Context(), config.Config{}, &repo, agentruntime.Default(), "", codexRunOptions{}, "", "", "", "", false)
	if code != ExitSuccess || err != nil || len(runner.exps) != 0 {
		t.Fatalf("resumed checks = (%d, %q, %v), remaining commands=%d", code, stage, err, len(runner.exps))
	}
}

func TestResumedCheckWaitClassifiesGitHubQueryErrors(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	cmd := prStateViewCommand("", url, "statusCheckRollup")
	queryErr := errors.New("exit status 1")
	for _, tc := range []struct {
		name, stderr string
		attempts     int
		retryable    bool
	}{
		{"forbidden", "HTTP 403: Resource not accessible", 1, false},
		{"unavailable", "HTTP 503: Service Unavailable", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{t: t}
			for range tc.attempts {
				runner.exps = append(runner.exps, expectedRun{cmd: cmd, res: execx.Result{Stderr: tc.stderr}, err: queryErr})
			}
			h := New(runner)
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			merged, err := h.waitForResumedChecks(t.Context(), url)
			if merged || !errors.Is(err, queryErr) || errors.Is(err, ErrChecksUnavailable) != tc.retryable ||
				!strings.Contains(err.Error(), tc.stderr) || len(runner.calls) != tc.attempts {
				t.Fatalf("remote wait = (%t, %v), queries = %d", merged, err, len(runner.calls))
			}
		})
	}
}

func TestRunReturnsSuccessWhenResumedChecksFindPRMerged(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/acme/repo/pull/42"
	runtime, err := agentruntime.Resolve("codex", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{t: t}
	for _, cmd := range preflightCommandsWithRuntime(runtime) {
		runner.exps = append(runner.exps, expectedRun{cmd: cmd})
	}
	runner.exps = append(runner.exps,
		expectedRun{cmd: authCommand()},
		expectedRun{cmd: prStateViewCommand("", url, "statusCheckRollup"), res: execx.Result{Stdout: `{"state":"MERGED"}`}},
	)
	h := New(runner)
	h.IssueChecksPRURL = url
	h.AcquireWorkspace = func(context.Context) error {
		t.Fatal("confirmed merge tried to acquire a workspace")
		return nil
	}
	result := h.Run(t.Context(), sampleConfig())
	if result.ExitCode != ExitSuccess || result.Err != nil || result.PRURL != url ||
		!result.PRAlreadyMerged || result.ReviewInterrupted || result.PRVerificationPassed {
		t.Fatalf("confirmed merge result = %+v", result)
	}
}
