package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (h Harness) readPRCheckRollup(ctx context.Context, repo repoWorkspace, id, expected string) ([]prCheckRollupEntry, error) {
	cmd := ghCommand(repo.Dir, "api", "graphql", "--paginate", "--jq", ".data.node", "-f", "id="+id, "-f", "query="+prCheckRollupQuery)
	res, err := h.runCheckQuery(ctx, "checks", cmd, checkQueryTimeout)
	if err != nil {
		return nil, fmt.Errorf("read pull request check rollup: %w", err)
	}

	var checks []prCheckRollupEntry
	decoder := json.NewDecoder(strings.NewReader(res.Stdout))
	for pageNumber := 0; ; pageNumber++ {
		var page struct {
			prStateView
			Commits struct {
				Nodes []struct {
					Commit struct {
						OID               string
						StatusCheckRollup struct {
							Contexts struct{ Nodes []prCheckRollupEntry }
						}
					}
				}
			}
		}
		if err := decoder.Decode(&page); err == io.EOF && pageNumber > 0 {
			return checks, nil
		} else if err != nil {
			return nil, fmt.Errorf("%w: decode pull request check rollup: %w", ErrChecksUnavailable, err)
		}
		if err := checkPRMatches(page.prStateView, repo, expected); err != nil {
			return nil, err
		}
		if len(page.Commits.Nodes) != 1 || page.Commits.Nodes[0].Commit.OID != expected {
			return nil, fmt.Errorf("%w: check rollup did not match the current pull request head", ErrChecksUnavailable)
		}
		checks = append(checks, page.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes...)
	}
}

func (h Harness) confirmCurrentRequiredChecks(ctx context.Context, repo repoWorkspace, expected string) error {
	state, err := h.readCheckPRState(ctx, prStateViewCommand(repo.Dir, repo.PRURL, "id"))
	if err != nil {
		return err
	}
	if err := checkPRMatches(state, repo, expected); err != nil {
		return err
	}
	if state.ID == "" {
		return fmt.Errorf("%w: pull request ID was missing", ErrChecksUnavailable)
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
	checks, err := h.readPRCheckRollup(ctx, repo, state.ID, expected)
	if err != nil {
		return err
	}
	passing := make(map[string]bool, len(required))
	for _, check := range checks {
		if !check.IsRequired {
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
