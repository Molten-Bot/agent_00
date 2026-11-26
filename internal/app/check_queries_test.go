package app

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

func TestCheckQueryHonorsSharedRateLimitDeadline(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(7 * time.Minute)
	cooldown := &githubutil.Cooldown{Now: func() time.Time { return now }}
	calls := 0
	h := New(issueBranchRunner(func(context.Context, execx.Command) (execx.Result, error) {
		calls++
		if calls == 1 {
			return execx.Result{Stdout: fmt.Sprintf("HTTP/2.0 403 Forbidden\r\nX-RateLimit-Remaining: 0\r\nX-RateLimit-Reset: %d\r\n\r\n{}", reset.Unix())}, errors.New("exit status 1")
		}
		return execx.Result{Stdout: "CI\tpass\n"}, nil
	}))
	h.GitHubCooldown = cooldown
	h.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("rate-limited worker held its slot waiting for the reset")
		return nil
	}
	watch := func() error {
		_, err := h.runPRChecksWatch(t.Context(), "/repo", "https://github.com/acme/repo/pull/42")
		return err
	}
	var rate *githubutil.RateLimitError
	if err := watch(); !errors.Is(err, ErrChecksUnavailable) || !errors.As(err, &rate) || !rate.RetryAt.Equal(reset) {
		t.Fatalf("first query = %v, want check wait until %v", err, reset)
	}
	if err := watch(); !errors.Is(err, ErrChecksUnavailable) || calls != 1 || !cooldown.RetryAt().Equal(reset) {
		t.Fatalf("query repeated before reset: err=%v calls=%d", err, calls)
	}
	now = reset
	if err := watch(); err != nil || calls != 2 {
		t.Fatalf("watch did not recover after reset: err=%v calls=%d", err, calls)
	}
}

func TestCheckQueryKeepsConfirmedFailureDistinctFromUnavailable(t *testing.T) {
	t.Parallel()
	failure := errors.New("exit status 1")
	h := New(issueBranchRunner(func(context.Context, execx.Command) (execx.Result, error) {
		return execx.Result{Stdout: "CI\tfail\t1s\n"}, failure
	}))
	h.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("a check failure was retried as a query error")
		return nil
	}
	result, err := h.runPRChecksWatch(t.Context(), "/repo", "https://github.com/acme/repo/pull/42")
	snapshot, parseErr := latestCheckSnapshotFromText(result.Stdout)
	if parseErr != nil || !snapshot.HasFailures || !errors.Is(err, failure) || errors.Is(err, ErrChecksUnavailable) {
		t.Fatalf("got snapshot=%+v query error=%v parse error=%v", snapshot, err, parseErr)
	}
}

func TestRequiredCheckConfirmationUsesCurrentHeadAndRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, head, rollup, want string
		nextPage                 string
		unavailable              bool
	}{
		{name: "current required results", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`},
		{name: "changed head", head: "older", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`, want: "head changed", unavailable: true},
		{name: "missing ruleset check", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`, want: "required check \"lint\"", unavailable: true},
		{name: "same-name optional pass with missing required check", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":false},{"context":"lint","state":"SUCCESS","isRequired":true}]`, want: "required check \"build\"", unavailable: true},
		{name: "same-name optional pass with pending required check", head: "current", rollup: `[{"name":"build","isRequired":true},{"name":"build","conclusion":"SUCCESS","isRequired":false},{"context":"lint","state":"SUCCESS","isRequired":true}]`, want: "required check \"build\"", unavailable: true},
		{name: "same-name optional pending with passing required check", head: "current", rollup: `[{"name":"build","isRequired":false},{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`},
		{name: "required check on later page", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`,
			nextPage: checkRollupPageJSON("task", "current", "current", `[{"context":"lint","state":"SUCCESS","isRequired":true}]`)},
		{name: "later page reports a different commit", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`,
			nextPage: checkRollupPageJSON("task", "current", "older", `[{"context":"lint","state":"SUCCESS","isRequired":true}]`),
			want:     "rollup did not match", unavailable: true},
		{name: "actual failed check", head: "current", rollup: `[{"name":"build","conclusion":"FAILURE","isRequired":true}]`, want: "failed on the current pull request head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/42", Branch: "task", BaseBranch: "main"}
			h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				args := strings.Join(cmd.Args, " ")
				switch {
				case strings.HasPrefix(args, "pr view "):
					return execx.Result{Stdout: fmt.Sprintf(`{"id":"PR_id","state":"OPEN","headRefName":"task","baseRefName":"main","headRefOid":%q}`, tc.head)}, nil
				case strings.HasPrefix(args, "api graphql --paginate --jq .data.node -f id=PR_id ") &&
					strings.Contains(args, "isRequired(pullRequestId: $id)"):
					return execx.Result{Stdout: checkRollupPageJSON("task", tc.head, tc.head, tc.rollup) + "\n" + tc.nextPage}, nil
				case strings.Contains(args, "/protection/required_status_checks"):
					return execx.Result{Stdout: `{"contexts":["build"]}`}, nil
				case strings.Contains(args, "/rules/branches/"):
					return execx.Result{Stdout: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"}]}}]`}, nil
				}
				t.Fatalf("unexpected check read: %s", args)
				return execx.Result{}, nil
			}))
			err := h.confirmCurrentRequiredChecks(t.Context(), repo, "current")
			if (err != nil) != (tc.want != "") || (err != nil && !strings.Contains(err.Error(), tc.want)) ||
				errors.Is(err, ErrChecksUnavailable) != tc.unavailable {
				t.Fatalf("confirmation error = %v, want %q (retryable=%t)", err, tc.want, tc.unavailable)
			}
		})
	}
}

func checkRollupPageJSON(branch, head, commit, rollup string) string {
	return fmt.Sprintf(`{"state":"OPEN","baseRefName":"main","headRefName":%q,"headRefOid":%q,
		"commits":{"nodes":[{"commit":{"oid":%q,"statusCheckRollup":{"contexts":{"nodes":%s}}}}]}}`,
		branch, head, commit, rollup)
}
