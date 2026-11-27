package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/tasktiming"
)

// ErrChecksUnavailable leaves verification pending when GitHub cannot supply
// current check evidence.
var ErrChecksUnavailable = errors.New("waiting for GitHub check verification")

const checkQueryTimeout = 30 * time.Second

type checkQueryRunner struct {
	h     Harness
	phase string
}

func (r checkQueryRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	return r.h.runCommandUnchecked(ctx, r.phase, cmd)
}

func (h Harness) runGitHubCommand(ctx context.Context, cooldown *githubutil.Cooldown, phase string, cmd execx.Command) (execx.Result, error) {
	for {
		res, err := cooldown.Run(ctx, checkQueryRunner{h, phase}, cmd, false)
		var rate *githubutil.RateLimitError
		if !h.WaitForGitHub || !errors.As(err, &rate) ||
			(!isGitHubReadCommand(cmd) && (!rate.Rejected || !isIndependentGitHubWrite(cmd))) {
			return res, err
		}
		h.logf("stage=%s status=waiting action=github_rate_limit retry_at=%s", phase, rate.RetryAt.UTC().Format(time.RFC3339))
		if err := waitForGitHubCooldown(ctx, cooldown, rate.RetryAt); err != nil {
			return res, errors.Join(rate, err)
		}
	}
}

func waitForGitHubCooldown(ctx context.Context, cooldown *githubutil.Cooldown, retryAt time.Time) error {
	defer tasktiming.Enter(ctx, tasktiming.GitHub)()
	return cooldown.Wait(ctx, retryAt)
}

func isGitHubReadCommand(cmd execx.Command) bool {
	if len(cmd.Args) < 2 {
		return false
	}
	switch cmd.Args[0] {
	case "auth":
		return cmd.Args[1] == "status"
	case "api":
		// Review writes require fresh head evidence before retrying.
		if index := slices.Index(cmd.Args, "--method"); index >= 0 && index+1 < len(cmd.Args) {
			return cmd.Args[index+1] == "GET"
		}
		if slices.Contains(cmd.Args, "graphql") {
			return slices.ContainsFunc(cmd.Args, func(arg string) bool {
				query, ok := strings.CutPrefix(arg, "query=")
				query = strings.TrimSpace(query)
				return ok && (strings.HasPrefix(query, "query") || strings.HasPrefix(query, "{"))
			})
		}
		return !slices.Contains(cmd.Args, "-f") && !slices.Contains(cmd.Args, "-F") && !slices.Contains(cmd.Args, "--input")
	case "issue", "pr", "repo", "run", "workflow":
		switch cmd.Args[1] {
		case "view", "list", "checks", "diff":
			return true
		}
	}
	return false
}

// These writes do not depend on previously read review evidence. A definite
// rejection allows retry without repeating a successful write.
func isIndependentGitHubWrite(cmd execx.Command) bool {
	switch {
	case len(cmd.Args) >= 4 && slices.Equal(cmd.Args[:3], []string{"api", "--method", "POST"}):
		return strings.HasSuffix(cmd.Args[3], "/labels") || strings.HasSuffix(cmd.Args[3], "/assignees")
	case len(cmd.Args) >= 2:
		switch cmd.Args[0] + " " + cmd.Args[1] {
		case "issue edit", "pr draft", "pr edit", "pr comment", "repo fork", "workflow run":
			return true
		case "pr ready":
			return slices.Contains(cmd.Args[2:], "--undo")
		}
	}
	return false
}

func (h Harness) runCheckQuery(ctx context.Context, phase string, cmd execx.Command, timeout time.Duration) (execx.Result, error) {
	sleep := h.Sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return execx.Result{}, ctx.Err()
		}
		queryCtx := ctx
		cancel := func() {}
		if timeout > 0 {
			queryCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		res, err := h.GitHubCooldown.Run(githubutil.WithCaller(queryCtx, "worker_checks"), checkQueryRunner{h, phase}, cmd, h.ResumeIssueWorkspace)
		cancel()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var rate *githubutil.RateLimitError
		if errors.As(err, &rate) {
			if h.WaitForGitHub && !h.ResumeIssueWorkspace {
				h.logf("stage=%s status=waiting action=github_rate_limit retry_at=%s", phase, rate.RetryAt.UTC().Format(time.RFC3339))
				if waitErr := waitForGitHubCooldown(ctx, h.GitHubCooldown, rate.RetryAt); waitErr == nil {
					attempt = -1
					continue
				} else {
					err = errors.Join(err, waitErr)
				}
			}
			// Let recovery schedule the next read. A server retry deadline can
			// outlive this worker's entire check budget.
			return res, fmt.Errorf("%w: %w", ErrChecksUnavailable, err)
		}
		if err == nil || !IsTransientGitHubReadError(fmt.Errorf("%w: %s", err, res.Stderr)) {
			return res, err
		}
		if attempt == 2 {
			return res, fmt.Errorf("%w: GitHub check query retries exhausted: %w", ErrChecksUnavailable, err)
		}
		delay := time.Second << attempt
		h.logf("stage=%s status=waiting reason=github_query_unavailable retry=%d/2 delay_s=%d", phase, attempt+1, int(delay.Seconds()))
		if err := sleep(ctx, delay); err != nil {
			return res, err
		}
	}
}

func (h Harness) loadCheckPRState(ctx context.Context, repoDir, prURL string) (prStateView, error) {
	return h.readCheckPRState(ctx, prStateViewCommand(repoDir, prURL))
}

func (h Harness) readCheckPRState(ctx context.Context, cmd execx.Command) (prStateView, error) {
	res, err := h.runCheckQuery(ctx, "checks", cmd, checkQueryTimeout)
	if err != nil {
		return prStateView{}, fmt.Errorf("read pull request head for checks: %w", err)
	}
	var state prStateView
	if err := json.Unmarshal([]byte(res.Stdout), &state); err != nil {
		return prStateView{}, fmt.Errorf("%w: decode pull request head for checks: %w", ErrChecksUnavailable, err)
	}
	return state, nil
}

func (h Harness) requireCheckHead(ctx context.Context, repo repoWorkspace, expected string) error {
	state, err := h.loadCheckPRState(ctx, repo.Dir, repo.PRURL)
	if err != nil {
		return err
	}
	return checkHeadMatches(state, expected)
}

func checkHeadMatches(state prStateView, expected string) error {
	if pullRequestStateIsMerged(state) {
		return githubutil.ErrReviewInterrupted
	}
	if state.State == "" {
		return fmt.Errorf("%w: pull request state was missing", ErrChecksUnavailable)
	}
	if !pullRequestStateIsOpen(state) {
		return fmt.Errorf("pull request must be open to verify checks")
	}
	if expected == "" || state.HeadRefOID != expected {
		return fmt.Errorf("%w: pull request head changed or is still propagating", ErrChecksUnavailable)
	}
	return nil
}

func checkPRMatches(state prStateView, repo repoWorkspace, expected string) error {
	if err := checkHeadMatches(state, expected); err != nil {
		return err
	}
	if state.BaseRefName != repo.BaseBranch || state.HeadRefName != repo.Branch {
		return fmt.Errorf("%w: pull request branches changed while verifying checks", ErrIssueBranchRecovery)
	}
	return nil
}

// Read GitHub's required flag alongside the commit OID so that a same-name
// optional check or a result from a different commit cannot satisfy the gate.
const prCheckRollupQuery = `query($id: ID!, $endCursor: String) {
  node(id: $id) {
    ... on PullRequest {
      state baseRefName headRefName headRefOid
      commits(last: 1) {
        nodes {
          commit {
            oid
            statusCheckRollup {
              contexts(first: 100, after: $endCursor) {
                pageInfo { hasNextPage endCursor }
                nodes {
                  ... on CheckRun { name conclusion isRequired(pullRequestId: $id) }
                  ... on StatusContext { context state isRequired(pullRequestId: $id) }
                }
              }
            }
          }
        }
      }
    }
  }
}`

type prCheckRollupEntry struct {
	githubutil.StatusCheck
	IsRequired bool
}

type prCheckRollupContexts struct {
	PageInfo struct {
		HasNextPage *bool
		EndCursor   string
	}
	Nodes []prCheckRollupEntry
}

func (h Harness) readPRCheckRollup(ctx context.Context, repo repoWorkspace, id, expected string) ([]prCheckRollupEntry, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: pull request ID was missing", ErrChecksUnavailable)
	}
	var checks []prCheckRollupEntry
	var cursor string
	seenCursors := make(map[string]bool)
	for {
		// Read each page through the check retry and rate-limit policy.
		cmd := ghCommand(repo.Dir, "api", "graphql", "-f", "id="+id, "-f", "query="+prCheckRollupQuery)
		if cursor != "" {
			cmd.Args = append(cmd.Args, "-f", "endCursor="+cursor)
		}
		if repo.ReviewSanitize {
			cmd = commandWithoutGitHubTokenEnv(cmd)
		}
		res, err := h.runCheckQuery(ctx, "checks", cmd, checkQueryTimeout)
		if err != nil {
			return nil, fmt.Errorf("read pull request check rollup: %w", err)
		}
		var response struct {
			Data struct {
				Node struct {
					prStateView
					Commits struct {
						Nodes []struct {
							Commit struct {
								OID               string
								StatusCheckRollup *struct {
									Contexts prCheckRollupContexts
								}
							}
						}
					}
				}
			}
		}
		if err := json.Unmarshal([]byte(res.Stdout), &response); err != nil {
			return nil, fmt.Errorf("%w: decode pull request check rollup: %w", ErrChecksUnavailable, err)
		}
		page := response.Data.Node
		if err := checkPRMatches(page.prStateView, repo, expected); err != nil {
			return nil, err
		}
		if len(page.Commits.Nodes) != 1 || page.Commits.Nodes[0].Commit.OID != expected {
			return nil, fmt.Errorf("%w: check rollup did not match the current pull request head", ErrChecksUnavailable)
		}
		rollup := page.Commits.Nodes[0].Commit.StatusCheckRollup
		if rollup == nil && cursor == "" {
			return nil, nil
		}
		if rollup == nil || rollup.Contexts.PageInfo.HasNextPage == nil {
			return nil, fmt.Errorf("%w: check rollup pagination metadata was missing", ErrChecksUnavailable)
		}
		checks = append(checks, rollup.Contexts.Nodes...)
		info := rollup.Contexts.PageInfo
		if !*info.HasNextPage {
			return checks, nil
		}
		if info.EndCursor == "" {
			return nil, fmt.Errorf("%w: check rollup pagination cursor was missing", ErrChecksUnavailable)
		}
		if seenCursors[info.EndCursor] {
			return nil, fmt.Errorf("%w: check rollup pagination cursor repeated", ErrChecksUnavailable)
		}
		cursor = info.EndCursor
		seenCursors[cursor] = true
	}
}

func (h Harness) confirmCurrentRequiredChecks(ctx context.Context, repo repoWorkspace, expected string) error {
	return h.confirmHeadChecks(ctx, repo, expected, false)
}

func (h Harness) confirmHeadChecks(ctx context.Context, repo repoWorkspace, expected string, ready bool) error {
	stateCmd := prStateViewCommand(repo.Dir, repo.PRURL, "id")
	if repo.ReviewSanitize {
		stateCmd = commandWithoutGitHubTokenEnv(stateCmd)
	}
	state, err := h.readCheckPRState(ctx, stateCmd)
	if err != nil {
		return err
	}
	if ready {
		if pullRequestStateIsMerged(state) {
			return githubutil.ErrReviewInterrupted
		}
		if state.HeadRefOID != expected {
			return githubutil.ErrReviewHeadChanged
		}
		// Journaled review completion can run after the checkout is removed.
		if repo.BaseBranch == "" {
			repo.BaseBranch = state.BaseRefName
		}
		if repo.Branch == "" {
			repo.Branch = state.HeadRefName
		}
		if repo.BaseBranch == "" || repo.Branch == "" {
			return fmt.Errorf("%w: pull request base or head branch was missing", ErrChecksUnavailable)
		}
	}
	if err := checkPRMatches(state, repo, expected); err != nil {
		return err
	}
	required, err := h.requiredCIStatusChecks(ctx, repo, ready)
	if err != nil {
		return err
	}
	checks, err := h.readPRCheckRollup(ctx, repo, state.ID, expected)
	if err != nil {
		return err
	}
	passing := make(map[string]bool, len(required))
	for _, check := range checks {
		if check.Name == githubutil.ReviewCheckName || check.Context == githubutil.ReviewCheckName {
			continue
		}
		if !ready && !check.IsRequired {
			continue
		}
		name := check.Name
		if name == "" {
			name = check.Context
		}
		passed, failed := githubutil.CIStatus([]githubutil.StatusCheck{check.StatusCheck})
		if failed {
			return fmt.Errorf("check %q failed on the current pull request head", name)
		}
		if ready && !passed {
			return fmt.Errorf("%w: check %q is still pending on the reviewed head", ErrChecksUnavailable, name)
		}
		if !check.IsRequired {
			continue
		}
		if previous, seen := passing[name]; seen {
			passed = passed && previous
		}
		passing[name] = passed
	}
	for _, name := range required {
		if !passing[name] {
			return fmt.Errorf("%w: required check %q has no passing result on the current head", ErrChecksUnavailable, name)
		}
	}
	return nil
}

func (h Harness) requiredCIStatusChecks(ctx context.Context, repo repoWorkspace, requireEvidence bool) ([]string, error) {
	target := githubutil.PullRequestRepository(repo.PRURL)
	required, err := h.configuredRequiredStatusChecks(ctx, repo.Dir, target, repo.BaseBranch, repo.ReviewSanitize, requireEvidence)
	if err != nil {
		return nil, fmt.Errorf("load required checks: %w", err)
	}
	rules, err := githubutil.RequiredBranchRuleStatusChecks(ctx, func(ctx context.Context, args ...string) (string, error) {
		cmd := ghCommand(repo.Dir, args...)
		if repo.ReviewSanitize {
			cmd = commandWithoutGitHubTokenEnv(cmd)
		}
		res, err := h.runCheckQuery(ctx, "checks", cmd, checkQueryTimeout)
		if requireEvidence && err == nil && strings.TrimSpace(res.Stdout) == "" {
			return "", fmt.Errorf("missing active branch rules")
		}
		return res.Stdout, err
	}, target, repo.BaseBranch)
	if err != nil {
		return nil, fmt.Errorf("load required ruleset checks: %w", err)
	}
	required = append(required, rules...)
	return slices.DeleteFunc(required, func(name string) bool { return name == githubutil.ReviewCheckName }), nil
}

// A preserved checkout can be behind a newer published commit. Advance only
// by fast-forwarding the reserved branch, and check the fetched object before
// using it for review or remediation.
func (h Harness) syncChecksResumeHead(ctx context.Context, repo repoWorkspace, expected string) error {
	if expected == "" {
		return fmt.Errorf("%w: GitHub has not reported the pull request head", ErrChecksUnavailable)
	}
	head, err := h.currentHead(ctx, repo)
	if err != nil || head == expected {
		return err
	}
	if res, err := h.runCommand(ctx, "git", fetchBranchFromRemoteCommand(repo.Dir, repoPushRemote(repo), "refs/heads/"+repo.Branch)); err != nil {
		if IsTransientGitHubReadError(fmt.Errorf("%w: %s", err, res.Stderr)) {
			return fmt.Errorf("%w: fetch the published PR head: %w", ErrChecksUnavailable, err)
		}
		return err
	}
	res, err := h.runCommand(ctx, "git", gitCommand(repo.Dir, "rev-parse", "FETCH_HEAD"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Stdout) != expected {
		return fmt.Errorf("%w: pull request metadata and published branch head differ", ErrChecksUnavailable)
	}
	if _, err := h.runCommand(ctx, "git", gitCommand(repo.Dir, "merge", "--ff-only", "--no-overwrite-ignore", expected)); err != nil {
		if errors.Is(err, ErrChecksUnavailable) {
			return err
		}
		return fmt.Errorf("%w: cannot fast-forward the saved checkout to the published pull request head: %v", ErrIssueBranchRecovery, err)
	}
	return nil
}
