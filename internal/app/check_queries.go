package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
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
	return r.h.runCommand(ctx, r.phase, cmd)
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
		res, err := h.GitHubCooldown.Run(queryCtx, checkQueryRunner{h, phase}, cmd, h.ResumeIssueWorkspace)
		cancel()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var rate *githubutil.RateLimitError
		if errors.As(err, &rate) {
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

func (h Harness) confirmCurrentRequiredChecks(ctx context.Context, repo repoWorkspace, expected string) error {
	state, err := h.readCheckPRState(ctx, prStateViewCommand(repo.Dir, repo.PRURL, "statusCheckRollup"))
	if err != nil {
		return err
	}
	if err := checkHeadMatches(state, expected); err != nil {
		return err
	}
	if state.BaseRefName != repo.BaseBranch || state.HeadRefName != repo.Branch {
		return fmt.Errorf("%w: pull request branches changed while verifying checks", ErrIssueBranchRecovery)
	}
	target := githubutil.PullRequestRepository(repo.PRURL)
	required, err := h.configuredRequiredStatusChecks(ctx, repo.Dir, target, repo.BaseBranch, false)
	if err != nil {
		return fmt.Errorf("load required checks: %w", err)
	}
	rules, err := githubutil.RequiredBranchRuleStatusChecks(ctx, func(ctx context.Context, args ...string) (string, error) {
		res, err := h.runCheckQuery(ctx, "checks", ghCommand(repo.Dir, args...), checkQueryTimeout)
		return res.Stdout, err
	}, target, repo.BaseBranch)
	if err != nil {
		return fmt.Errorf("load required ruleset checks: %w", err)
	}
	required = append(required, rules...)
	passing := make(map[string]bool, len(state.StatusCheckRollup))
	for _, check := range state.StatusCheckRollup {
		name := check.Name
		if name == "" {
			name = check.Context
		}
		passed, failed := githubutil.CIStatus([]githubutil.StatusCheck{check})
		if failed {
			return fmt.Errorf("check %q failed on the current pull request head", name)
		}
		if previous, seen := passing[name]; seen {
			passed = passed && previous
		}
		passing[name] = passed
	}
	for _, name := range required {
		if name != githubutil.ReviewCheckName && !passing[name] {
			return fmt.Errorf("%w: required check %q has no passing result on the current head", ErrChecksUnavailable, name)
		}
	}
	return nil
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
