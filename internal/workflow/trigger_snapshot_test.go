package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

func TestWorkflowPollingPreservesRunsAndObservationsDuringMemoryPressure(t *testing.T) {
	t.Parallel()
	w := definition("issues")
	w.IncludeBacklog = true
	e, items, submitted := testEngine(t, w)
	*items = []Item{{Number: 1, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}}}
	fetch := e.Fetch
	polls := 0
	e.Fetch = func(ctx context.Context, repo string, needs ScanNeeds) ([]Item, error) {
		polls++
		return fetch(ctx, repo, needs)
	}
	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("initial runs = %+v, %v", runs, err)
	}
	paused := true
	e.MemoryPressure = func() bool { return paused }
	*items = append(*items, Item{Number: 2, Repository: w.Repository, Kind: "issue", Open: true, Labels: []string{"ready"}})
	tick(t, e)
	if polls != 1 || *submitted != 1 {
		t.Fatalf("paused workflow: %d polls, %d submissions", polls, *submitted)
	}
	if err := e.Complete(runs[0].ID, Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	runs, err = e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Outcome == nil || runs[0].Outcome.Status != "succeeded" {
		t.Fatalf("recording the outcome while paused: runs=%+v, err=%v", runs, err)
	}
	paused = false
	tick(t, e)
	tick(t, e)
	runs, err = e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Item.Number != 2 || *submitted != 2 {
		t.Fatalf("resumed workflow: runs=%+v, submissions=%d, err=%v", runs, *submitted, err)
	}
}

func TestIssueTriggerContinuesWhenPullRequestSnapshotFails(t *testing.T) {
	t.Parallel()
	issueWorkflow := definition("issue")
	prWorkflow := definition("pr")
	prWorkflow.Trigger = Trigger{Event: "pr.opened"}
	prWorkflow.Task.Kind = "review"
	prWorkflow.IncludeBacklog = true
	e, _, _ := testEngine(t, issueWorkflow, prWorkflow)
	ctx := context.Background()
	viewErr := errors.New("temporary pull request inspection failure")
	broken := false
	polls := 0
	var submitted []string
	e.Submit = func(_ context.Context, r Run) (string, error) {
		submitted = append(submitted, r.Workflow.ID)
		return r.Workflow.ID, nil
	}
	e.Fetch = (GitHub{Runner: fakeRunner(func(_ context.Context, command execx.Command) (execx.Result, error) {
		args := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(args, "/pulls?"):
			return execx.Result{Stdout: `[{"number":2}]`}, nil
		case strings.Contains(args, "/issues?"):
			polls++
			if broken {
				return execx.Result{Stdout: `[{"number":2,"pull_request":{}},{"number":1,"labels":[{"name":"ready"}]}]`}, nil
			}
			return execx.Result{Stdout: `[{"number":2,"pull_request":{}}]`}, nil
		case strings.HasPrefix(args, "pr view"):
			if broken {
				return execx.Result{}, viewErr
			}
			return execx.Result{Stdout: `{"number":2,"state":"OPEN","headRefOid":"abc"}`}, nil
		case strings.HasPrefix(args, "api graphql"):
			return execx.Result{Stdout: "0"}, nil
		case strings.Contains(args, "/reviews?"):
			return execx.Result{Stdout: "[]"}, nil
		case strings.HasPrefix(args, "api user"):
			return execx.Result{Stdout: "agent"}, nil
		default:
			t.Fatalf("unexpected GitHub command: %s", args)
			return execx.Result{}, nil
		}
	})}).Fetch

	tick(t, e)
	runs, err := e.Runs()
	if err != nil || len(runs) != 1 || runs[0].Workflow.ID != prWorkflow.ID {
		t.Fatalf("initial runs = %+v, %v", runs, err)
	}
	if err := e.Complete(runs[0].ID, Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	broken = true
	if err := e.Tick(ctx); !errors.Is(err, viewErr) {
		t.Fatalf("tick error = %v, want pull request inspection failure", err)
	}
	if polls != 2 || strings.Join(submitted, ",") != "pr,issue" {
		t.Fatalf("polls = %d, submitted workflows = %v", polls, submitted)
	}
	if decisions, err := e.Preview(ctx, issueWorkflow); err != nil || len(decisions) != 1 || !decisions[0].Matches {
		t.Fatalf("issue preview = %+v, %v", decisions, err)
	}
	if _, err := e.Preview(ctx, prWorkflow); !errors.Is(err, viewErr) {
		t.Fatalf("pull request preview error = %v", err)
	}
	broken = false
	tick(t, e)
	if strings.Join(submitted, ",") != "pr,issue" {
		t.Fatalf("pull request recovery duplicated work: %v", submitted)
	}
}

func TestOnlyEnabledTriggersRequestGitHubData(t *testing.T) {
	t.Parallel()
	issues := definition("issues")
	disabled := definition("disabled")
	disabled.Enabled = false
	disabled.Trigger = Trigger{Event: "pr.review_requested", Value: "@me"}
	disabled.Task.Kind = "review"
	e, _, _ := testEngine(t, issues, disabled)
	calls := 0
	e.Fetch = (GitHub{Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		calls++
		if !strings.Contains(strings.Join(cmd.Args, " "), "/issues?") {
			t.Fatalf("unneeded GitHub call %v", cmd.Args)
		}
		return execx.Result{Stdout: `[{"number":1,"pull_request":{}}]`}, nil
	})}).Fetch
	tick(t, e)
	if calls != 1 {
		t.Fatalf("issue-only poll made %d GitHub calls", calls)
	}
	e.Enabled = func() bool { return false }
	tick(t, e)
	if calls != 1 {
		t.Fatal("disabled feature polled GitHub")
	}
}

func TestBasicPRTriggerUsesOnlyPullsCollection(t *testing.T) {
	t.Parallel()
	w := definition("prs")
	w.Trigger = Trigger{Event: "pr.review_requested", Value: "reviewer"}
	w.Task.Kind = "review"
	w.IncludeBacklog = true
	e, _, submitted := testEngine(t, w)
	e.Fetch = (GitHub{Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if !strings.Contains(strings.Join(cmd.Args, " "), "/pulls?") {
			t.Fatalf("extra GitHub lookup: %v", cmd.Args)
		}
		return execx.Result{Stdout: `[{"number":4,"head":{"ref":"topic","sha":"abc"},"base":{"ref":"main"},"requested_reviewers":[{"login":"reviewer"}]}]`}, nil
	})}).Fetch
	tick(t, e)
	if *submitted != 1 {
		t.Fatal("missed review request")
	}
}

func TestIssueLabelScanCost(t *testing.T) {
	t.Parallel()
	for _, count := range []int{1, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			t.Parallel()
			w := definition("issues")
			w.Trigger.Value = "@me" // A literal label, even when it resembles a login.
			e, _, _ := testEngine(t, w)
			var rows []string
			for n := 1; n <= count; n++ {
				rows = append(rows, fmt.Sprintf(`{"number":%d,"pull_request":{}}`, n))
			}
			calls := 0
			e.Fetch = (GitHub{Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				calls++
				if strings.Join(cmd.Args, " ") != "api --paginate repos/acme/repo/issues?state=open&per_page=100" {
					t.Fatalf("unexpected lookup %v", cmd.Args)
				}
				return execx.Result{Stdout: "[" + strings.Join(rows, ",") + "]"}, nil
			})}).Fetch
			ds, err := e.Preview(context.Background(), w)
			if err != nil || len(ds) != 0 || calls != 1 {
				t.Fatalf("preview decisions=%v calls=%d err=%v", ds, calls, err)
			}
			tick(t, e)
			if calls != 2 {
				t.Fatalf("poll made %d calls", calls-1)
			}
		})
	}
}

func TestMixedWorkflowsShareCollectionListings(t *testing.T) {
	t.Parallel()
	issue := definition("issues")
	pr := definition("prs")
	pr.Trigger = Trigger{Event: "pr.ready"}
	pr.Task.Kind = "review"
	issue.IncludeBacklog, pr.IncludeBacklog = true, true
	other := pr
	other.ID = "other"
	e, _, submitted := testEngine(t, issue, pr, other)
	var calls []string
	var logs []string
	e.Fetch = (GitHub{Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, Runner: fakeRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		args := strings.Join(cmd.Args, " ")
		switch {
		case strings.Contains(args, "/issues?"):
			calls = append(calls, "issues")
			return execx.Result{Stdout: `[{"number":1,"labels":[{"name":"ready"}]},{"number":2,"pull_request":{}}]`}, nil
		case strings.Contains(args, "/pulls?"):
			calls = append(calls, "pulls")
			return execx.Result{Stdout: `[{"number":2,"head":{"ref":"topic","sha":"abc"}}]`}, nil
		default:
			t.Fatalf("unexpected detail lookup %v", cmd.Args)
			return execx.Result{}, nil
		}
	})}).Fetch
	tick(t, e)
	if strings.Join(calls, ",") != "issues,pulls" || *submitted != 2 {
		t.Fatalf("calls=%v submissions=%d", calls, *submitted)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "gh_calls=2 duration_ms=") {
		t.Fatalf("scan logs=%v", logs)
	}
}

func TestWorkflowDiscoveryDoesNotConsumeEventDuringQuotaPause(t *testing.T) {
	t.Parallel()
	w := definition("quota-discovery")
	e, _, submitted := testEngine(t, w)
	now := time.Now().Truncate(time.Second)
	reset := now.Add(time.Minute)
	e.Now = func() time.Time { return now }
	cooldown := &githubutil.Cooldown{Now: e.Now, Logf: t.Logf}
	calls := 0
	runner := fakeRunner(func(_ context.Context, _ execx.Command) (execx.Result, error) {
		calls++
		remaining, deadline := 100, reset
		items := `[{"number":1,"labels":[{"name":"ready"}]}]`
		switch calls {
		case 2:
			remaining = 0
		case 3, 4:
			deadline = now.Add(time.Hour)
			items = `[{"number":1,"labels":[{"name":"ready"}]},{"number":2,"labels":[{"name":"ready"}]}]`
		}
		return execx.Result{Stdout: items, Stderr: fmt.Sprintf(
			"* Request to https://api.github.com/repos/acme/repo/issues\n< HTTP/2.0 200 OK\n< X-RateLimit-Resource: core\n< X-RateLimit-Limit: 5000\n< X-RateLimit-Remaining: %d\n< X-RateLimit-Reset: %d\n\n* Request took 1ms\n",
			remaining, deadline.Unix())}, nil
	})
	e.Fetch = (GitHub{Runner: runner, Cooldown: cooldown}).Fetch
	tick(t, e)
	now = now.Add(30 * time.Second)
	for range 2 {
		var rate *githubutil.RateLimitError
		if err := e.Tick(t.Context()); !errors.As(err, &rate) || !rate.RetryAt.Equal(reset) {
			t.Fatalf("discovery cooldown = %v", err)
		}
	}
	if calls != 2 || *submitted != 0 {
		t.Fatalf("during quota pause: calls=%d submitted=%d", calls, *submitted)
	}
	now = reset
	tick(t, e)
	now = now.Add(30 * time.Second)
	tick(t, e)
	runs, err := e.Runs()
	if *submitted != 1 || len(runs) != 1 || runs[0].Item.Number != 2 || err != nil {
		t.Fatalf("resumed discovery: submitted=%d runs=%+v err=%v", *submitted, runs, err)
	}
}
