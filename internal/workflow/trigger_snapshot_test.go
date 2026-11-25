package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

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
