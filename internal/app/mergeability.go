package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

const (
	maxMergeabilityChecks  = 6
	mergeabilityRetryDelay = 2 * time.Second
	maxPRConflictRepairs   = 3
)

var errPRMergeConflict = errors.New("pull request has merge conflicts")

type prMergeability struct {
	prStateView
	Mergeable string `json:"mergeable"`
}

func prMergeabilityCommand(repoDir, prURL string) execx.Command {
	return ghCommand(repoDir, "pr", "view", prURL, "--json", "state,mergeable,baseRefName,headRefName,headRefOid")
}

// GitHub computes mergeability asynchronously, including after pushes and base updates.
func (h Harness) probePullRequestMergeability(ctx context.Context, repo repoWorkspace, expectedHead string, afterRepair bool) (prMergeability, error) {
	sleep := h.Sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	cmd := prMergeabilityCommand(repo.Dir, repo.PRURL)
	if repo.ReviewSanitize {
		cmd = commandWithoutGitHubTokenEnv(cmd)
	}
	var state prMergeability
	for check := 0; check < maxMergeabilityChecks; check++ {
		if check > 0 {
			if err := sleep(ctx, mergeabilityRetryDelay); err != nil {
				return state, err
			}
		}
		res, err := h.runCheckQuery(ctx, "git", cmd, checkQueryTimeout)
		if err != nil {
			return state, fmt.Errorf("check pull request mergeability: %w", err)
		}
		state = prMergeability{}
		if err := json.Unmarshal([]byte(res.Stdout), &state); err != nil {
			return state, fmt.Errorf("decode pull request mergeability: %w", err)
		}
		state.Mergeable = strings.ToUpper(state.Mergeable)
		if !pullRequestStateIsOpen(state.prStateView) {
			return state, nil
		}
		if expectedHead != "" && state.HeadRefOID != expectedHead {
			if afterRepair {
				continue
			}
			if state.HeadRefOID != "" && state.Mergeable != "CONFLICTING" {
				return state, errCheckHeadMismatch
			}
		}
		switch state.Mergeable {
		case "MERGEABLE":
			return state, nil
		case "CONFLICTING":
			if !afterRepair {
				return state, nil
			}
			// Allow GitHub to recompute after the repair push.
		case "UNKNOWN":
		default:
			return state, fmt.Errorf("unexpected pull request mergeability %q", state.Mergeable)
		}
	}
	return state, fmt.Errorf("%w: pull request mergeability could not be confirmed after %d checks", ErrChecksUnavailable, maxMergeabilityChecks)
}

func (h Harness) checkForPRConflict(ctx context.Context, repo repoWorkspace, expectedHead string) error {
	state, err := h.probePullRequestMergeability(ctx, repo, expectedHead, false)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChecksUnavailable, err)
	}
	if pullRequestStateIsMerged(state.prStateView) {
		return githubutil.ErrReviewInterrupted
	}
	if pullRequestStateIsOpen(state.prStateView) {
		if state.Mergeable == "CONFLICTING" {
			return errPRMergeConflict
		}
	} else if !strings.EqualFold(state.State, "CLOSED") {
		return fmt.Errorf("%w: unexpected pull request state %q during mergeability check", ErrChecksUnavailable, state.State)
	}
	return nil
}

func (h Harness) ensurePullRequestMergeable(
	ctx context.Context, cfg config.Config, repo *repoWorkspace,
	runtime agentruntime.Runtime, codexDir string, codexOpts codexRunOptions,
	codexBasePrompt, agentsPath, codexTargetLabel, agentStage string,
	expectedHead string, beforeChecks bool,
) (int, string, error) {
	if repo.PRAlreadyMerged {
		return ExitSuccess, "", nil
	}
	h.logf("stage=git status=start action=check_mergeability pr_url=%s", repo.PRURL)
	state, err := h.probePullRequestMergeability(ctx, *repo, expectedHead, false)
	if err != nil {
		return ExitPR, "pr", err
	}
	if pullRequestStateIsOpen(state.prStateView) && state.Mergeable == "CONFLICTING" {
		repairStarted := time.Now()
		h.logf("stage=merge_conflict status=start action=detect pr_url=%s repo_dir=%s head=%s base_branch=%s",
			repo.PRURL, repo.RelDir, state.HeadRefOID, state.BaseRefName)
		base := normalizeBranchRef(state.BaseRefName)
		if base == "" || normalizeBranchRef(state.HeadRefName) != normalizeBranchRef(repo.Branch) || base == normalizeBranchRef(repo.Branch) {
			return ExitGit, "git", fmt.Errorf("cannot repair pull request conflicts: invalid base or mismatched head branch")
		}
		if repo.MergeConflictRepairs >= maxPRConflictRepairs {
			return ExitPR, "pr", fmt.Errorf("pull request still has merge conflicts after %d repairs", repo.MergeConflictRepairs)
		}
		if err := h.requireWorkspace(ctx); err != nil {
			return ExitWorkspace, "workspace", err
		}
		h.step("merge_conflict")
		if state.HeadRefOID != "" {
			if err := h.syncChecksResumeHead(ctx, *repo, state.HeadRefOID); err != nil {
				return ExitGit, "git", err
			}
		}
		// Use the PR's actual target and push to its existing branch.
		syncRepo := *repo
		syncRepo.BaseBranch = base
		syncRepo.CreateWorkBranch = true
		if code, stage, err := h.syncGeneratedWorkBranchWithBase(ctx, &syncRepo, runtime, codexDir, codexOpts, codexBasePrompt, agentsPath, cfg.ResponseMode, codexTargetLabel, agentStage); err != nil {
			return code, stage, err
		}
		syncRepo.CreateWorkBranch = false
		if code, stage, err := h.pushWithSyncAndConflictResolution(ctx, &syncRepo, 0, runtime, codexDir, codexOpts, codexBasePrompt, agentsPath, cfg.ResponseMode, codexTargetLabel, agentStage); err != nil {
			return code, stage, err
		}
		repo.Changed = true
		repo.MergeConflictRepairs++
		repo.ReviewReceipt = nil
		repo.ReviewedPatch.remove()
		repo.ReviewedPatch = nil
		head, err := h.currentHead(ctx, *repo)
		if err != nil {
			return ExitGit, "git", err
		}
		h.logf("stage=merge_conflict status=ok action=publish_repair pr_url=%s repo_dir=%s head=%s elapsed_ms=%d",
			repo.PRURL, repo.RelDir, head, time.Since(repairStarted).Milliseconds())
		if h.ResumeIssueWorkspace && repo.IssueClaimBranch && repo.ReviewRepairPass > 0 {
			// Preserve the fix budget if GitHub is still computing mergeability
			// when the next attempt opens this repaired head.
			if err := h.savePublishedReviewRepair(*repo, head, capturedReviewPolicy(cfg, h.FinalReviewPasses)); err != nil {
				return ExitPR, "pr", err
			}
		}
		state, err = h.probePullRequestMergeability(ctx, *repo, head, true)
		if err != nil {
			return ExitPR, "pr", err
		}
	}
	if pullRequestStateIsMerged(state.prStateView) {
		h.logf("stage=pr status=ok action=observe_merge pr_url=%s", repo.PRURL)
		h.recordObservedMerge(repo)
		return ExitSuccess, "", nil
	}
	// The checks path can recreate a closed generated PR. Handoff requires
	// a confirmed open, mergeable PR.
	if !pullRequestStateIsOpen(state.prStateView) && !(beforeChecks && strings.EqualFold(state.State, "CLOSED")) {
		return ExitPR, "pr", fmt.Errorf("cannot verify mergeability of pull request in state %q", state.State)
	}
	h.logf("stage=git status=ok action=check_mergeability pr_url=%s", repo.PRURL)
	return ExitSuccess, "", nil
}
