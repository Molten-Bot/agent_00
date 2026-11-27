package app

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func TestPRCheckWatchRetainsPendingEvidenceWhenQueryTimesOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const url = "https://github.com/example/repo/pull/42"
		const pending = "Go\tpass\nBrowser tests\tpending\n"
		reads := 0
		h := New(issueBranchRunner(func(ctx context.Context, _ execx.Command) (execx.Result, error) {
			reads++
			if reads == 1 {
				return execx.Result{Stdout: pending}, pendingCheckExit{}
			}
			<-ctx.Done()
			return execx.Result{}, ctx.Err()
		}))
		h.CheckTimings = &CheckTimings{}
		h.PRChecksWatchTimeout = 15 * time.Second
		res, err := h.runPRChecksWatch(t.Context(), prChecksCommand("/checkout", url), newCheckPoll("checks", url))
		if reads != 2 || !errors.Is(err, errPRChecksWatchTimeout) || res.Stdout != pending {
			t.Fatalf("watch after %d reads = (%q, %v), want pending evidence and timeout", reads, res.Stdout, err)
		}
	})
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
	const open = `{"id":"PR_id","state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current","statusCheckRollup":`
	const passing = `[{"name":"CI","conclusion":"SUCCESS"}]`
	cmd := prStateViewCommand("", url, "statusCheckRollup", "id")
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
			final: open + passing + "}", classic: `["CI"]`, rules: "[]", run: "[]"},
		{name: "optional pass with missing ruleset check", initial: `[{"name":"optional","conclusion":"SUCCESS"}]`,
			final: open + passing + "}", classic: "[]", rules: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"CI"}]}}]`, run: "[]"},
		{name: "optional pass with no required checks and pending dispatch",
			initial: `[{"name":"optional","conclusion":"SUCCESS"}]`, classic: "[]", rules: "[]",
			final: `{"state":"MERGED"}`, merged: true,
			run: `[{"headSha":"current","status":"in_progress"}]`},
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
			if strings.Contains(tc.final, passing) {
				runner.exps = append(runner.exps, expectedRun{cmd: ghCommand("", "api", "graphql", "-f", "id=PR_id", "-f", "query="+prCheckRollupQuery),
					res: execx.Result{Stdout: checkRollupPageJSON("task", "current", "current", `[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`, false, "")}})
				runner.exps = append(runner.exps, expectedRun{cmd: dispatch, res: execx.Result{Stdout: "[]"}})
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
	t.Run("no required checks", func(t *testing.T) {
		testPublishedResumeReconcilesEmptyRollup(t, `{"contexts":[]}`, `[]`, true)
	})
	t.Run("required result in paginated rollup", func(t *testing.T) {
		testPublishedResumeReconcilesEmptyRollup(t, `{"contexts":["CI"]}`, `[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`, true)
	})
	t.Run("required result without dispatch", func(t *testing.T) {
		testPublishedResumeReconcilesEmptyRollup(t, `{"contexts":["CI"]}`, `[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`, false)
	})
}

func testPublishedResumeReconcilesEmptyRollup(t *testing.T, required, rollup string, dispatched bool) {
	t.Helper()
	const url = "https://github.com/example/repo/pull/42"
	repo := repoWorkspace{Dir: "/checkout", PRURL: url, BaseBranch: "main", Branch: "task", IssueClaimBranch: true}
	const state = `{"id":"PR_id","state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current"}`
	protection := expectedRun{cmd: requiredStatusChecksCommand(repo.Dir, "example/repo", "main"),
		res: execx.Result{Stdout: required}}
	rules := expectedRun{cmd: ghCommand(repo.Dir, "api", "--paginate", "repos/example/repo/rules/branches/main?per_page=100"),
		res: execx.Result{Stdout: "[]"}}
	dispatch := workflowDispatchRunsCommand(repo.Dir, repo.Branch)
	dispatch.Args = append(dispatch.Args, "--repo", "example/repo", "--commit", "current")
	runs := "[]"
	if dispatched {
		runs = `[{"headSha":"current","status":"completed","conclusion":"success"}]`
	}
	runner := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "current"}},
		{cmd: prStateViewCommand(repo.Dir, url), res: execx.Result{Stdout: state}},
		{cmd: prChecksCommand(repo.Dir, url), res: execx.Result{Stderr: "no checks reported"}, err: errors.New("exit status 1")},
		{cmd: dispatch, res: execx.Result{Stdout: runs}},
		{cmd: prStateViewCommand(repo.Dir, url, "id"), res: execx.Result{Stdout: state}},
		protection, rules,
		{cmd: ghCommand(repo.Dir, "api", "graphql", "-f", "id=PR_id", "-f", "query="+prCheckRollupQuery),
			res: execx.Result{Stdout: checkRollupPageJSON(repo.Branch, "current", "current", rollup, false, "")}},
	}}
	h := New(runner)
	h.IssueChecksPRURL = url
	h.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("resumed task slept after current-head checks were confirmed")
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
	cmd := prStateViewCommand("", url, "statusCheckRollup", "id")
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
	for _, duringAdmission := range []bool{false, true} {
		t.Run(fmt.Sprintf("during admission=%t", duringAdmission), func(t *testing.T) {
			t.Parallel()
			runtime, err := agentruntime.Resolve("codex", "")
			if err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{t: t}
			for _, cmd := range preflightCommandsWithRuntime(runtime) {
				runner.exps = append(runner.exps, expectedRun{cmd: cmd})
			}
			const merged = `{"state":"MERGED"}`
			initial := merged
			if duringAdmission {
				initial = `{"state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current","statusCheckRollup":[{"name":"CI","conclusion":"SUCCESS"}]}`
			}
			runner.exps = append(runner.exps, expectedRun{cmd: authCommand()},
				expectedRun{cmd: prStateViewCommand("", url, "statusCheckRollup", "id"), res: execx.Result{Stdout: initial}})
			if duringAdmission {
				dispatch := workflowDispatchRunsCommand("", "task")
				dispatch.Args = append(dispatch.Args, "--repo", "acme/repo", "--commit", "current")
				runner.exps = append(runner.exps,
					expectedRun{cmd: requiredStatusChecksCommand("", "acme/repo", "main"), res: execx.Result{Stdout: `{"contexts":[]}`}},
					expectedRun{cmd: ghCommand("", "api", "--paginate", "repos/acme/repo/rules/branches/main?per_page=100"), res: execx.Result{Stdout: "[]"}},
					expectedRun{cmd: dispatch, res: execx.Result{Stdout: "[]"}},
					expectedRun{cmd: prStateViewCommand("", url), res: execx.Result{Stdout: merged}},
				)
			}
			h := New(runner)
			h.IssueChecksPRURL = url
			h.Workspace.CheckSpace = nil
			h.Workspace.MkdirAll = func(string, os.FileMode) error { return nil }
			h.Workspace.WriteFile = func(string, []byte, os.FileMode) error { return nil }
			held := false
			h.AcquireWorkspace = func(context.Context) error {
				if !duringAdmission || held {
					t.Fatal("unexpected workspace admission")
				}
				held = true
				return nil
			}
			h.ReleaseWorkspace = func() {
				if !held {
					t.Fatal("unexpected workspace release")
				}
				held = false
			}
			result := h.Run(t.Context(), sampleConfig())
			if result.ExitCode != ExitSuccess || result.Err != nil || result.PRURL != url || held ||
				!result.PRAlreadyMerged || result.ReviewInterrupted || result.PRVerificationPassed || len(runner.exps) != 0 {
				t.Fatalf("confirmed merge result = %+v, workspace held = %t, remaining queries = %d", result, held, len(runner.exps))
			}
		})
	}
}

func TestResumedCheckReconciliationUsesRemainingDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const url = "https://github.com/example/repo/pull/42"
		const state = `{"id":"PR_id","state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current"}`
		const timeout = 5 * time.Second
		start := time.Now()
		finalView := prStateViewCommand("", url, "id")
		dispatch := workflowDispatchRunsCommand("", "task")
		dispatch.Args = append(dispatch.Args, "--repo", "example/repo", "--commit", "current")
		runner := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prStateViewCommand("", url, "statusCheckRollup", "id"), res: execx.Result{Stdout: state}},
			{cmd: requiredStatusChecksCommand("", "example/repo", "main"), res: execx.Result{Stdout: `{"contexts":["CI"]}`}},
			{cmd: ghCommand("", "api", "--paginate", "repos/example/repo/rules/branches/main?per_page=100"), res: execx.Result{Stdout: "[]"}},
			{cmd: dispatch, res: execx.Result{Stdout: "[]"}},
			{cmd: finalView},
		}}
		h := New(issueBranchRunner(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
			res, err := runner.Run(ctx, cmd)
			if len(runner.exps) > 0 {
				return res, err
			}
			if got := time.Since(start); got <= 0 || got >= timeout {
				t.Fatalf("reconciliation started after %v, want time left in %v budget", got, timeout)
			}
			if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(start.Add(timeout)) {
				t.Fatalf("reconciliation deadline = %v, want %v", deadline, start.Add(timeout))
			}
			<-ctx.Done()
			return execx.Result{}, ctx.Err()
		}))
		h.PRChecksWatchTimeout = timeout
		merged, err := h.waitForResumedChecks(t.Context(), url)
		if merged || !errors.Is(err, ErrChecksUnavailable) || time.Since(start) != timeout || len(runner.exps) != 0 {
			t.Fatalf("remote check wait = (%t, %v) after %v, remaining queries=%d", merged, err, time.Since(start), len(runner.exps))
		}
	})
}

func TestResumedCheckWaitRemembersDisappearedCheckUntilHeadChangesOrCheckReturns(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	cmd := prStateViewCommand("", url, "statusCheckRollup", "id")
	view := func(head, rollup string) expectedRun {
		return expectedRun{cmd: cmd, res: execx.Result{Stdout: fmt.Sprintf(
			`{"state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":%q,"statusCheckRollup":%s}`, head, rollup)}}
	}
	dispatch := func(head string) expectedRun {
		cmd := workflowDispatchRunsCommand("", "task")
		cmd.Args = append(cmd.Args, "--repo", "example/repo", "--commit", head)
		return expectedRun{cmd: cmd, res: execx.Result{Stdout: "[]"}}
	}
	for _, tc := range []struct {
		name  string
		final []expectedRun
	}{
		{"check returns", []expectedRun{view("current", `[{"name":"CI","conclusion":"SUCCESS"}]`), dispatch("current")}},
		{"new head", []expectedRun{view("new", "[]"), dispatch("new")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{t: t, exps: append([]expectedRun{
				view("current", `[{"name":"CI","status":"IN_PROGRESS"}]`),
				view("current", "[]"),
				{cmd: requiredStatusChecksCommand("", "example/repo", "main"), res: execx.Result{Stdout: `{"contexts":[]}`}},
				{cmd: ghCommand("", "api", "--paginate", "repos/example/repo/rules/branches/main?per_page=100"), res: execx.Result{Stdout: "[]"}},
				dispatch("current"),
			}, tc.final...)}
			h := New(runner)
			waits := 0
			var progress string
			h.Logf = func(format string, args ...any) { progress = fmt.Sprintf(format, args...) }
			h.Sleep = func(context.Context, time.Duration) error {
				waits++
				if !strings.Contains(progress, `pending_checks="CI"`) {
					t.Fatalf("missing check name in progress: %s", progress)
				}
				return nil
			}
			if merged, err := h.waitForResumedChecks(t.Context(), url); merged || err != nil || waits != 2 || len(runner.exps) != 0 {
				t.Fatalf("remote check wait = (%t, %v), waits=%d, remaining queries=%d", merged, err, waits, len(runner.exps))
			}
		})
	}
}

func TestResumedReconcilesMissingRequiredChecks(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	const state = `{"id":"PR_id","state":"OPEN","baseRefName":"main","headRefName":"task","headRefOid":"current"}`
	const completed = `[{"headSha":"current","status":"completed","conclusion":"success","workflowName":"CI workflow"}]`
	policy := []expectedRun{
		{cmd: requiredStatusChecksCommand("", "example/repo", "main"), res: execx.Result{Stdout: `{"contexts":["CI"]}`}},
		{cmd: ghCommand("", "api", "--paginate", "repos/example/repo/rules/branches/main?per_page=100"), res: execx.Result{Stdout: "[]"}},
	}
	dispatch := workflowDispatchRunsCommand("", "task")
	dispatch.Args = append(dispatch.Args, "--repo", "example/repo", "--commit", "current")
	for _, tc := range []struct {
		name, state, checks                  string
		merged, retry                        bool
		partial, rerun, noDispatch, sameName bool
	}{
		{name: "still missing", state: state, checks: "[]"},
		{name: "optional result cannot satisfy required check", state: state, partial: true,
			checks: `[{"name":"CI","conclusion":"SUCCESS","isRequired":false}]`},
		{name: "PR-triggered result on second page", state: state, partial: true, noDispatch: true,
			checks: `[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`},
		{name: "PR-triggered result absent from CLI rollup", state: state, noDispatch: true,
			checks: `[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`},
		{name: "same-name optional result cannot readmit workspace", state: state, noDispatch: true, sameName: true, retry: true,
			checks: `[{"name":"CI","conclusion":"SUCCESS","isRequired":false}]`},
		{name: "result appeared", state: state, checks: `[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`},
		{name: "check still pending", state: state, checks: `[{"name":"CI","isRequired":true}]`, retry: true},
		{name: "dispatch rerun started", state: state, retry: true, rerun: true},
		{name: "merged", state: `{"state":"MERGED"}`, merged: true},
		{name: "head changed", state: strings.ReplaceAll(state, "current", "new"), retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				initial := state
				if tc.partial {
					initial = strings.TrimSuffix(state, "}") + `,"statusCheckRollup":[{"name":"optional","conclusion":"SUCCESS"}]}`
				}
				if tc.sameName {
					initial = strings.TrimSuffix(state, "}") + `,"statusCheckRollup":[{"name":"CI","conclusion":"SUCCESS"}]}`
				}
				runs := completed
				if tc.noDispatch {
					runs = "[]"
				}
				rollupCmd := ghCommand("", "api", "graphql", "-f", "id=PR_id", "-f", "query="+prCheckRollupQuery)
				runner := &fakeRunner{t: t, exps: []expectedRun{
					{cmd: prStateViewCommand("", url, "statusCheckRollup", "id"), res: execx.Result{Stdout: initial}},
				}}
				runner.exps = append(runner.exps, policy...)
				if tc.sameName {
					runner.exps = append(runner.exps, expectedRun{cmd: rollupCmd,
						res: execx.Result{Stdout: checkRollupPageJSON("task", "current", "current", tc.checks, false, "")}})
				}
				runner.exps = append(runner.exps, expectedRun{cmd: dispatch, res: execx.Result{Stdout: runs}})
				runner.exps = append(runner.exps, expectedRun{cmd: prStateViewCommand("", url, "id"), res: execx.Result{Stdout: tc.state}})
				if tc.state == state {
					if tc.rerun {
						runs = `[{"headSha":"current","status":"in_progress"}]`
					}
					runner.exps = append(runner.exps, expectedRun{cmd: dispatch, res: execx.Result{Stdout: runs}})
				}
				if tc.checks != "" {
					runner.exps = append(runner.exps, policy...)
					if tc.partial && tc.noDispatch {
						runner.exps = append(runner.exps, expectedRun{cmd: rollupCmd,
							res: execx.Result{Stdout: checkRollupPageJSON("task", "current", "current", `[{"name":"optional","conclusion":"SUCCESS"}]`, true, "next")}})
						rollupCmd.Args = append(append([]string(nil), rollupCmd.Args...), "-f", "endCursor=next")
					}
					runner.exps = append(runner.exps, expectedRun{cmd: rollupCmd,
						res: execx.Result{Stdout: checkRollupPageJSON("task", "current", "current", tc.checks, false, "")}})
				}
				h := New(runner)
				h.PRChecksWatchTimeout = 5 * time.Second
				start := time.Now()
				merged, err := h.waitForResumedChecks(t.Context(), url)
				if merged != tc.merged || errors.Is(err, ErrChecksUnavailable) != tc.retry || len(runner.exps) != 0 || time.Since(start) != 4*time.Second {
					t.Fatalf("remote check wait = (%t, %v) after %v, remaining queries=%d", merged, err, time.Since(start), len(runner.exps))
				}
				if tc.checks == "[]" || (tc.partial && !tc.noDispatch) {
					if err == nil || !strings.Contains(err.Error(), `"CI workflow" completed, but required checks are missing on current head current: CI`) {
						t.Fatalf("missing actionable failure: %v", err)
					}
				} else if !tc.retry && err != nil {
					t.Fatalf("unexpected failure: %v", err)
				}
			})
		})
	}
}
