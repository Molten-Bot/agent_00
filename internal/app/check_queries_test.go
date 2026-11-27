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
		const url = "https://github.com/acme/repo/pull/42"
		_, err := h.runPRChecksWatch(t.Context(), prChecksCommand("/repo", url), newCheckPoll("checks", url))
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

func TestDraftRestorationConfirmsAmbiguousWrite(t *testing.T) {
	t.Parallel()
	commands := 0
	h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		commands++
		if len(cmd.Args) >= 4 && cmd.Args[0] == "pr" && cmd.Args[1] == "ready" && cmd.Args[3] == "--undo" {
			return execx.Result{}, errors.New("connection lost after write")
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "view" {
			return execx.Result{Stdout: `{"isDraft":true,"state":"OPEN"}`}, nil
		}
		return execx.Result{}, fmt.Errorf("unexpected command: %v", cmd.Args)
	}))
	repo := repoWorkspace{Dir: t.TempDir(), PRURL: "https://github.com/acme/repo/pull/42"}
	if err := h.ensurePullRequestDraft(t.Context(), repo, "pr_verification"); err != nil || commands != 2 {
		t.Fatalf("ambiguous draft restoration = %v after %d calls", err, commands)
	}
}

func TestDraftRestorationIsRetryableAfterDefiniteRateLimitRejection(t *testing.T) {
	t.Parallel()
	cmd := prDraftCommand("/repo", "https://github.com/acme/repo/pull/42")
	if !isIndependentGitHubWrite(cmd) {
		t.Fatalf("draft restoration is not classified as a retryable write: %v", cmd.Args)
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
	const url = "https://github.com/acme/repo/pull/42"
	result, err := h.runPRChecksWatch(t.Context(), prChecksCommand("/repo", url), newCheckPoll("checks", url))
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
		more                     bool
		unavailable              bool
		ready, noRequired        bool
		hiddenProtection         bool
	}{
		{name: "current required results", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`},
		{name: "changed head", head: "older", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`, want: "head changed", unavailable: true},
		{name: "missing ruleset check", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`, want: "required check \"lint\"", unavailable: true},
		{name: "same-name optional pass with missing required check", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":false},{"context":"lint","state":"SUCCESS","isRequired":true}]`, want: "required check \"build\"", unavailable: true},
		{name: "same-name optional pass with pending required check", head: "current", rollup: `[{"name":"build","isRequired":true},{"name":"build","conclusion":"SUCCESS","isRequired":false},{"context":"lint","state":"SUCCESS","isRequired":true}]`, want: "required check \"build\"", unavailable: true},
		{name: "same-name optional pending with passing required check", head: "current", rollup: `[{"name":"build","isRequired":false},{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`},
		{name: "optional failures with passing required checks", head: "current", rollup: `[{"name":"build","conclusion":"FAILURE","isRequired":false},{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true},{"context":"preview","state":"FAILURE","isRequired":false}]`},
		{name: "same-name optional failure with missing required check", head: "current", rollup: `[{"name":"build","conclusion":"FAILURE","isRequired":false},{"context":"lint","state":"SUCCESS","isRequired":true}]`, want: "required check \"build\"", unavailable: true},
		{name: "required check on later page", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`,
			more: true, nextPage: checkRollupPageJSON("task", "current", "current", `[{"context":"lint","state":"SUCCESS","isRequired":true}]`, false, "")},
		{name: "later required failure blocks passing results from first page", head: "current",
			rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`,
			more:   true, nextPage: checkRollupPageJSON("task", "current", "current", `[{"name":"build","conclusion":"FAILURE","isRequired":true}]`, false, ""),
			want: `check "build" failed on the current pull request head`},
		{name: "later page reports a different commit", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`,
			more: true, nextPage: checkRollupPageJSON("task", "current", "older", `[{"context":"lint","state":"SUCCESS","isRequired":true}]`, false, ""),
			want: "rollup did not match", unavailable: true},
		{name: "later page missing despite passing first page", head: "current",
			rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`,
			more:   true, want: "decode pull request check rollup", unavailable: true},
		{name: "later page repeats cursor", head: "current", rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`,
			more: true, nextPage: checkRollupPageJSON("task", "current", "current", `[{"context":"lint","state":"SUCCESS","isRequired":true}]`, true, "page-2"),
			want: "pagination cursor repeated", unavailable: true},
		{name: "actual failed check", head: "current", rollup: `[{"name":"build","conclusion":"FAILURE","isRequired":true}]`, want: "failed on the current pull request head"},
		{name: "ready with all requirements present", head: "current", ready: true, rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true},{"context":"lint","state":"SUCCESS","isRequired":true}]`},
		{name: "ready with no configured checks", head: "current", ready: true, noRequired: true, rollup: "[]"},
		{name: "ready blocked when branch protection is unavailable", head: "current", ready: true, hiddenProtection: true,
			want: "load required checks: required_status_checks HTTP 404"},
		{name: "ready with empty rollup and configured checks", head: "current", ready: true, rollup: "[]", want: `required check "build"`, unavailable: true},
		{name: "ready with review-only rollup and configured checks", head: "current", ready: true,
			rollup: `[{"name":"` + githubutil.ReviewCheckName + `","conclusion":"SUCCESS","isRequired":true}]`, want: `required check "build"`, unavailable: true},
		{name: "ready with only classic requirement present", head: "current", ready: true,
			rollup: `[{"name":"build","conclusion":"SUCCESS","isRequired":true}]`, want: `required check "lint"`, unavailable: true},
		{name: "ready with optional check still pending", head: "current", ready: true, noRequired: true,
			rollup: `[{"name":"preview","isRequired":false}]`, want: `check "preview" is still pending`, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/42", Branch: "task", BaseBranch: "main"}
			rollupReads := 0
			h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				args := strings.Join(cmd.Args, " ")
				switch {
				case strings.HasPrefix(args, "pr view "):
					return execx.Result{Stdout: fmt.Sprintf(`{"id":"PR_id","state":"OPEN","headRefName":"task","baseRefName":"main","headRefOid":%q}`, tc.head)}, nil
				case strings.HasPrefix(args, "api graphql -f id=PR_id ") &&
					strings.Contains(args, "isRequired(pullRequestId: $id)"):
					rollupReads++
					if rollupReads == 1 && !strings.Contains(args, "endCursor=") {
						return execx.Result{Stdout: checkRollupPageJSON("task", tc.head, tc.head, tc.rollup, tc.more, "page-2")}, nil
					}
					if rollupReads == 2 && tc.more && strings.HasSuffix(args, " -f endCursor=page-2") {
						return execx.Result{Stdout: tc.nextPage}, nil
					}
				case strings.Contains(args, "/protection/required_status_checks"):
					if tc.hiddenProtection {
						return execx.Result{}, errors.New("required_status_checks HTTP 404")
					}
					if tc.noRequired {
						return execx.Result{Stdout: `{"contexts":[]}`}, nil
					}
					return execx.Result{Stdout: `{"contexts":["build"]}`}, nil
				case strings.Contains(args, "/rules/branches/"):
					if tc.noRequired {
						return execx.Result{Stdout: `[]`}, nil
					}
					return execx.Result{Stdout: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"}]}}]`}, nil
				}
				t.Fatalf("unexpected check read: %s", args)
				return execx.Result{}, nil
			}))
			var err error
			if tc.ready {
				repo.BaseBranch, repo.Branch = "", ""
				err = h.confirmHeadChecks(t.Context(), repo, "current", true)
			} else {
				err = h.confirmCurrentRequiredChecks(t.Context(), repo, "current")
			}
			if (err != nil) != (tc.want != "") || (err != nil && !strings.Contains(err.Error(), tc.want)) ||
				errors.Is(err, ErrChecksUnavailable) != tc.unavailable {
				t.Fatalf("confirmation error = %v, want %q (retryable=%t)", err, tc.want, tc.unavailable)
			}
		})
	}
}

func checkRollupPageJSON(branch, head, commit, rollup string, hasNextPage bool, endCursor string) string {
	return fmt.Sprintf(`{"data":{"node":{"state":"OPEN","baseRefName":"main","headRefName":%q,"headRefOid":%q,
		"commits":{"nodes":[{"commit":{"oid":%q,"statusCheckRollup":{"contexts":{
			"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":%s}}}}]}}}}`,
		branch, head, commit, hasNextPage, endCursor, rollup)
}
