package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
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
	if _, err := h.runPRChecksWatch(t.Context(), "/checkout", url); err != nil {
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

func TestResumedCheckWaitUsesOnlyGitHubUntilChecksSettle(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/example/repo/pull/42"
	cmd := prStateViewCommand("", url, "statusCheckRollup")
	for _, tc := range []struct {
		name, final string
		merged      bool
		want        error
	}{
		{"passing", `{"state":"OPEN","statusCheckRollup":[{"name":"CI","conclusion":"SUCCESS"}]}`, false, nil},
		{"merged", `{"state":"MERGED"}`, true, nil},
		{"pending", `{"state":"OPEN","statusCheckRollup":[{"name":"CI","status":"IN_PROGRESS"}]}`, false, ErrChecksUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{t: t, exps: []expectedRun{
				{cmd: cmd, res: execx.Result{Stdout: `{"state":"OPEN","statusCheckRollup":[{"name":"CI","status":"IN_PROGRESS"}]}`}},
				{cmd: cmd, res: execx.Result{Stdout: tc.final}},
			}}
			h := New(runner)
			h.CheckTimings = &CheckTimings{}
			const estimate = 2 * time.Minute
			key := newCheckPoll("checks", url).key
			h.CheckTimings.record(key, estimate)
			h.PRChecksWatchTimeout = time.Second
			h.Sleep = func(_ context.Context, delay time.Duration) error {
				if delay != time.Second {
					t.Fatalf("delay = %v, want 1s budget", delay)
				}
				return nil
			}
			h.AcquireWorkspace = func(context.Context) error {
				t.Fatal("remote check wait tried to acquire workspace")
				return nil
			}
			if merged, err := h.waitForResumedChecks(t.Context(), url); merged != tc.merged || !errors.Is(err, tc.want) {
				t.Fatalf("remote check wait = (%t, %v), want (%t, %v)", merged, err, tc.merged, tc.want)
			}
			if got := h.CheckTimings.average(key); got != estimate {
				t.Fatalf("resumed wait changed build estimate to %v, want %v", got, estimate)
			}
		})
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
