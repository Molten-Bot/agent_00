package web

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

type stubPRMonitorRunner struct {
	mu       sync.Mutex
	result   execx.Result
	err      error
	results  []execx.Result
	errs     []error
	commands []execx.Command
}

func (s *stubPRMonitorRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	idx := len(s.commands) - 1
	if idx < len(s.results) {
		var err error
		if idx < len(s.errs) {
			err = s.errs[idx]
		}
		return s.results[idx], err
	}
	return s.result, s.err
}

func (s *stubPRMonitorRunner) Commands() []execx.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

func (m *PRMergeMonitor) checkTaskPR(ctx context.Context, task Task) {
	state, err := m.prState(ctx, task.PRURL)
	m.applyPRState(ctx, task, state, err)
}

func TestDefaultPRMergePollIntervalIsLowLatency(t *testing.T) {
	t.Parallel()

	if defaultPRMergePollInterval > 10*time.Second {
		t.Fatalf("defaultPRMergePollInterval = %s, want low-latency merge updates", defaultPRMergePollInterval)
	}
}

func TestPRMergeMonitorBoundsStateLookup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		killed := errors.New("signal: killed")
		m := PRMergeMonitor{
			Runner: githubIssueRunnerFunc(func(ctx context.Context, _ execx.Command) (execx.Result, error) {
				<-ctx.Done()
				return execx.Result{}, killed
			}),
		}
		start := time.Now()
		_, err := m.prState(t.Context(), "https://github.com/acme/repo/pull/42")
		if elapsed := time.Since(start); elapsed != prMonitorStateLookupTimeout || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, killed) {
			t.Fatalf("PR lookup ran for %s with error %v", elapsed, err)
		}
	})
}

func TestPRMergeMonitorDefersPollingAndCleanupDuringMemoryPressure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const requestID = "req-merged"
		broker := NewBroker()
		broker.IngestLog("task status=start request_id=" + requestID)
		broker.IngestLog("task status=completed request_id=" + requestID + " pr_url=https://github.com/acme/repo/pull/42")
		runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-04-09T12:00:00Z"}`}}
		paused := true
		cleaned := ""
		m := PRMergeMonitor{
			Runner: runner, Broker: broker, Logf: t.Logf,
			MemoryPressure: func() bool { return paused },
			CleanupTask: func(_ context.Context, id string) error {
				cleaned = id
				return nil
			},
		}
		m.pollOnce(t.Context())
		synctest.Wait()
		task, ok := broker.Task(requestID)
		if !ok || task.Status != "completed" || len(runner.Commands()) != 0 || cleaned != "" {
			t.Fatalf("paused monitor changed task %+v or cleaned %q", task, cleaned)
		}
		paused = false
		m.pollOnce(t.Context())
		synctest.Wait()
		task, ok = broker.Task(requestID)
		if !ok || task.Status != "merged" || len(runner.Commands()) != 1 || cleaned != requestID {
			t.Fatalf("resumed monitor left task %+v and cleaned %q", task, cleaned)
		}
	})
}

func TestPRMergeMonitorRefreshesDisplayedPRTitle(t *testing.T) {
	t.Parallel()

	const requestID = "req-multi-repo"
	const trackedURL = "https://github.com/acme/repo/pull/42"
	const displayedURL = "https://github.com/acme/other/pull/99"
	broker := NewBroker()
	now := time.Date(2026, time.April, 9, 12, 0, 0, 0, time.UTC)
	broker.now = func() time.Time { return now }
	broker.IngestLog("task status=start request_id=" + requestID)
	broker.RecordCreatedTaskPRTitle(requestID, displayedURL, "Original title")
	broker.IngestLog("task status=completed request_id=" + requestID + " pr_url=" + trackedURL)
	task, _ := broker.Task(requestID)
	verifiedAt := now.Add(time.Minute)
	now = now.Add(2 * time.Minute)

	runner := &stubPRMonitorRunner{
		results: []execx.Result{
			{Stdout: `{"state":"OPEN","title":"Tracked PR title"}`},
			{Stdout: "Renamed title\n"},
		},
	}
	monitor := PRMergeMonitor{Runner: runner, Broker: broker, Logf: t.Logf}
	monitor.checkTaskPR(t.Context(), task)

	broker.recordVerifiedTaskPR(requestID, verifiedTaskPR{URL: displayedURL, Title: "Original title", VerifiedAt: verifiedAt})
	got := broker.StreamSnapshot().Tasks[0]
	if got.PRURL != trackedURL || got.PRTitleURL != displayedURL || got.PRTitle != "Renamed title" || got.Status != "completed" {
		t.Fatalf("task after PR title refresh = %+v", got)
	}
	if got.PRTitleUpdatedAt != now.Format(time.RFC3339Nano) || got.UpdatedAt != task.UpdatedAt {
		t.Fatalf("title timestamp = %q and task timestamp = %q, want %s and %s",
			got.PRTitleUpdatedAt, got.UpdatedAt, now.Format(time.RFC3339Nano), task.UpdatedAt)
	}
	commands := runner.Commands()
	if want := []string{"pr", "view", displayedURL, "--json", "title", "--jq", ".title"}; len(commands) != 2 || !slices.Equal(commands[1].Args, want) {
		t.Fatalf("commands = %+v, want displayed PR lookup %v", commands, want)
	}
}

func TestPRMergeMonitorCleansUpWhenDisplayedPRTitleLookupTimesOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const requestID = "req-stalled-title"
		const trackedURL = "https://github.com/acme/repo/pull/42"
		const displayedURL = "https://github.com/acme/other/pull/99"
		broker := NewBroker()
		broker.IngestLog("task status=start request_id=" + requestID)
		broker.RecordCreatedTaskPRTitle(requestID, displayedURL, "Existing title")
		broker.IngestLog("task status=completed request_id=" + requestID + " pr_url=" + trackedURL)
		task, _ := broker.Task(requestID)

		var lookupErr error
		runner := githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
			switch cmd.Args[2] {
			case "42":
				return execx.Result{Stdout: `{"state":"MERGED"}`}, nil
			case displayedURL:
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Fatal("display-only title lookup needs a deadline within two seconds")
				}
				<-ctx.Done()
				lookupErr = ctx.Err()
				return execx.Result{}, lookupErr
			default:
				t.Fatalf("unexpected command: %+v", cmd)
				return execx.Result{}, nil
			}
		})
		var cleaned string
		monitor := PRMergeMonitor{
			Runner: runner,
			Broker: broker,
			Logf:   t.Logf,
			CleanupTask: func(ctx context.Context, id string) error {
				if err := ctx.Err(); err != nil {
					t.Fatalf("cleanup context is canceled: %v", err)
				}
				cleaned = id
				return nil
			},
		}
		monitor.checkTaskPR(t.Context(), task)

		got, ok := broker.Task(requestID)
		if lookupErr != context.DeadlineExceeded || cleaned != requestID || !ok || got.Status != "merged" || got.PRTitle != "Existing title" {
			t.Fatalf("lookup error = %v, cleaned task = %q, task = %+v", lookupErr, cleaned, got)
		}
	})
}

func TestPRMergeMonitorMarksMergedTaskDoneAndRunsCleanup(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.RecordTaskRunConfig("req-merged", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"ship it"}`))
	broker.IngestLog("dispatch status=start request_id=req-merged repo=git@github.com:acme/repo.git")
	broker.IngestLog("dispatch status=completed request_id=req-merged workspace=/tmp/run branch=agent00-ship pr_url=https://github.com/acme/repo/pull/42")

	runner := &stubPRMonitorRunner{
		result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-04-09T12:00:00Z"}`},
	}

	cleanupCalls := make(chan string, 1)
	monitor := PRMergeMonitor{
		Runner:       runner,
		Broker:       broker,
		PollInterval: 10 * time.Millisecond,
		CleanupTask: func(_ context.Context, requestID string) error {
			cleanupCalls <- requestID
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- monitor.Run(ctx)
	}()

	waitForHubUITest(t, 5*time.Second, func() bool {
		return len(runner.Commands()) > 0
	})

	select {
	case requestID := <-cleanupCalls:
		if got, want := requestID, "req-merged"; got != want {
			t.Fatalf("cleanup requestID = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected automatic cleanup after merged PR observation")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor.Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for monitor shutdown")
	}

	if _, ok := broker.TaskRunConfig("req-merged"); ok {
		t.Fatal("TaskRunConfig() found = true after merged PR observation, want false")
	}

	snap := broker.Snapshot()
	if got, want := len(snap.Tasks), 1; got != want {
		t.Fatalf("len(tasks) after merged PR observation = %d, want %d", got, want)
	}
	if got, want := snap.Tasks[0].Status, "merged"; got != want {
		t.Fatalf("task.Status after merged PR observation = %q, want %q", got, want)
	}
	if got, want := snap.Tasks[0].Stage, "finalize"; got != want {
		t.Fatalf("task.Stage after merged PR observation = %q, want %q", got, want)
	}
	if got, want := len(snap.Releases), 1; got != want {
		t.Fatalf("len(releases) after merged PR observation = %d, want %d", got, want)
	}
	if got, want := snap.Releases[0].Prompt, "ship it"; got != want {
		t.Fatalf("release.Prompt = %q, want %q", got, want)
	}
	if got, want := snap.Releases[0].PRURL, "https://github.com/acme/repo/pull/42"; got != want {
		t.Fatalf("release.PRURL = %q, want %q", got, want)
	}
	if got, want := snap.Releases[0].MergedAt, "2026-04-09T12:00:00Z"; got != want {
		t.Fatalf("release.MergedAt = %q, want %q", got, want)
	}

	commands := runner.Commands()
	if len(commands) == 0 {
		t.Fatal("gh command was not executed")
	}
	if got, want := commands[0].Name, "gh"; got != want {
		t.Fatalf("command name = %q, want %q", got, want)
	}
	if got, want := commands[0].Args, []string{"pr", "view", "42", "--json", "state,mergedAt,title", "--repo", "acme/repo"}; !slices.Equal(got, want) {
		t.Fatalf("command args = %v, want %v", got, want)
	}
}

func TestPRMergeMonitorKeepsTaskVisibleUntilPRIsMerged(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	broker.now = func() time.Time { return now }
	broker.IngestLog("dispatch status=start request_id=req-open repo=git@github.com:acme/repo.git")
	now = now.Add(2 * time.Minute)
	broker.IngestLog("dispatch status=completed request_id=req-open workspace=/tmp/run branch=agent00-ship pr_url=https://github.com/acme/repo/pull/77")
	completedAt := now.Format(time.RFC3339Nano)
	now = now.Add(time.Minute)

	runner := &stubPRMonitorRunner{
		result: execx.Result{Stdout: `{"state":"OPEN","mergedAt":"","title":"Published PR title"}`},
	}

	monitor := PRMergeMonitor{
		Runner:       runner,
		Broker:       broker,
		PollInterval: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- monitor.Run(ctx)
	}()

	waitForHubUITest(t, 5*time.Second, func() bool {
		task, ok := broker.Task("req-open")
		return ok && task.PRTitle == "Published PR title"
	})

	snap := broker.Snapshot()
	if got, want := len(snap.Tasks), 1; got != want {
		t.Fatalf("len(tasks) = %d, want %d", got, want)
	}
	if got, want := snap.Tasks[0].RequestID, "req-open"; got != want {
		t.Fatalf("task request_id = %q, want %q", got, want)
	}
	if got := snap.Tasks[0].UpdatedAt; got != completedAt {
		t.Errorf("task.UpdatedAt after PR title update = %q, want %q", got, completedAt)
	}
	if got, want := snap.Tasks[0].DurationSeconds, 120.0; got != want {
		t.Errorf("task.DurationSeconds after PR title update = %v, want %v", got, want)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor.Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for monitor shutdown")
	}
}

func TestPRMergeMonitorDoesNotReplaceReviewTaskTitle(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.RecordTaskRunConfigWithSource("req-review", []byte(`{"repo":"git@github.com:acme/repo.git","libraryTaskName":"code-review","review":{"prNumber":117}}`), "review")
	broker.IngestLog("dispatch status=start request_id=req-review repo=git@github.com:acme/repo.git")
	broker.IngestLog("dispatch status=no_changes request_id=req-review workspace=/tmp/run branch=feature pr_url=https://github.com/acme/repo/pull/117")

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"OPEN","title":"Published PR title"}`}}
	monitor := PRMergeMonitor{Runner: runner, Broker: broker, Logf: func(string, ...any) {}}

	original, ok := broker.Task("req-review")
	if !ok || original.Prompt == "" {
		t.Fatal("expected review task with a prompt")
	}
	monitor.checkTaskPR(t.Context(), original)

	task, _ := broker.Task("req-review")
	if task.PRTitle != "" || task.Prompt != original.Prompt {
		t.Fatalf("review task prompt = %q, PR title = %q, want prompt %q and no PR title", task.Prompt, task.PRTitle, original.Prompt)
	}

	runner.result = execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-05-31T12:00:00Z","title":"Published PR title"}`}
	monitor.checkTaskPR(t.Context(), task)
	task, _ = broker.Task("req-review")
	if task.Status != "merged" || task.PRTitle != "" {
		t.Fatalf("review task status = %q, PR title = %q, want merged and no PR title", task.Status, task.PRTitle)
	}
}

func TestPRMergeMonitorLogsCheckFailuresAndKeepsTask(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.IngestLog("dispatch status=start request_id=req-fail repo=git@github.com:acme/repo.git")
	broker.IngestLog("dispatch status=completed request_id=req-fail workspace=/tmp/run branch=agent00-ship pr_url=https://github.com/acme/repo/pull/99")

	runner := &stubPRMonitorRunner{err: errors.New("gh failed")}
	logs := make(chan string, 8)
	monitor := PRMergeMonitor{
		Runner:       runner,
		Broker:       broker,
		PollInterval: 10 * time.Millisecond,
		Logf: func(format string, args ...any) {
			select {
			case logs <- fmt.Sprintf(format, args...):
			default:
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- monitor.Run(ctx)
	}()

	waitForHubUITest(t, 5*time.Second, func() bool {
		return len(runner.Commands()) > 0
	})

	snap := broker.Snapshot()
	if got, want := len(snap.Tasks), 1; got != want {
		t.Fatalf("len(tasks) = %d, want %d", got, want)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor.Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for monitor shutdown")
	}

	select {
	case line := <-logs:
		if line == "" {
			t.Fatal("expected non-empty log line")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected warning log for failed PR status check")
	}
}

func waitForHubUITest(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestPRMonitorSharesReadsAndWaitsForQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := NewBroker()
		for i := 0; i < 8; i++ {
			id := fmt.Sprintf("req-%d", i)
			broker.IngestLog("task status=start request_id=" + id)
			broker.IngestLog(fmt.Sprintf("task status=completed request_id=%s pr_url=https://github.com/acme/repo/pull/%d", id, 1+i%4))
		}
		runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"OPEN"}`}}
		cooldown := &githubutil.Cooldown{}
		m := PRMergeMonitor{Broker: broker, Runner: runner, Cooldown: cooldown, Logf: t.Logf}
		poll := func() { m.pollOnce(t.Context()); synctest.Wait() }
		poll()
		poll()
		if got := len(runner.Commands()); got != 8 {
			t.Fatalf("two ticks across four PRs made %d calls", got)
		}
		for _, cmd := range runner.Commands() {
			if cmd.Args[0] != "pr" || cmd.Args[1] != "view" || cmd.Args[4] != "state,mergedAt,title" {
				t.Fatalf("unexpected lookup %v", cmd.Args)
			}
		}
		runner.err = errors.New("secondary rate limit")
		poll()
		calls := len(runner.Commands())
		if cooldown.RetryAt().IsZero() {
			t.Fatal("missing retry deadline")
		}
		poll()
		if len(runner.Commands()) != calls {
			t.Fatal("called GitHub during cooldown")
		}
		runner.err = nil
		time.Sleep(time.Minute)
		poll()
		if got := len(runner.Commands()) - calls; got != 4 {
			t.Fatalf("recovery made %d calls", got)
		}
	})
}
