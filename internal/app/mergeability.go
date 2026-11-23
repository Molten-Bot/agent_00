package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func prMergeabilityCommand(repoDir, prURL string) execx.Command {
	return ghCommand(repoDir, "pr", "view", prURL, "--json", "state,mergeable,baseRefName,headRefName")
}

// GitHub computes mergeability asynchronously. Unknown results are retried,
// but neither an unknown result nor an unresolved conflict is success.
func (h Harness) ensurePullRequestMergeable(
	ctx context.Context, cfg config.Config, repo *repoWorkspace,
	runtime agentruntime.Runtime, codexDir string, codexOpts codexRunOptions,
	codexBasePrompt, agentsPath, codexTargetLabel, agentStage string,
) (int, string, error) {
	const maxChecks = 6
	repaired := false
	for check := 0; check < maxChecks; check++ {
		if check > 0 {
			sleep := h.Sleep
			if sleep == nil {
				sleep = sleepWithContext
			}
			if err := sleep(ctx, 2*time.Second); err != nil {
				return ExitPR, "pr", err
			}
		}
		h.logf("stage=git status=start action=check_mergeability pr_url=%s attempt=%d", repo.PRURL, check+1)
		res, err := h.runCommand(ctx, "pr", prMergeabilityCommand(repo.Dir, repo.PRURL))
		if err != nil {
			return ExitPR, "pr", fmt.Errorf("check pull request mergeability: %w", err)
		}
		var state struct {
			State       string `json:"state"`
			Mergeable   string `json:"mergeable"`
			BaseRefName string `json:"baseRefName"`
			HeadRefName string `json:"headRefName"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &state); err != nil {
			return ExitPR, "pr", fmt.Errorf("decode pull request mergeability: %w", err)
		}
		if strings.EqualFold(state.State, "MERGED") {
			repo.ReviewStoppedOnMerge = true
			return ExitSuccess, "", nil
		}
		if !strings.EqualFold(state.State, "OPEN") {
			return ExitPR, "pr", fmt.Errorf("cannot verify mergeability of pull request in state %q", state.State)
		}
		switch strings.ToUpper(state.Mergeable) {
		case "MERGEABLE":
			h.logf("stage=git status=ok action=check_mergeability pr_url=%s", repo.PRURL)
			return ExitSuccess, "", nil
		case "UNKNOWN":
			continue
		case "CONFLICTING":
			if repaired {
				// Allow GitHub to recompute after the repair push.
				continue
			}
		default:
			return ExitPR, "pr", fmt.Errorf("unexpected pull request mergeability %q", state.Mergeable)
		}
		base := normalizeBranchRef(state.BaseRefName)
		if base == "" || normalizeBranchRef(state.HeadRefName) != normalizeBranchRef(repo.Branch) || base == normalizeBranchRef(repo.Branch) {
			return ExitGit, "git", fmt.Errorf("cannot repair pull request conflicts: invalid base or mismatched head branch")
		}
		// Existing PRs need the same conflict remediation as generated branches,
		// using the PR's actual target rather than a configured clone branch.
		syncRepo := *repo
		syncRepo.BaseBranch = base
		syncRepo.CreateWorkBranch = true
		if code, stage, err := h.syncGeneratedWorkBranchWithBase(ctx, &syncRepo, runtime, codexDir, codexOpts, codexBasePrompt, agentsPath, cfg.ResponseMode, codexTargetLabel, agentStage); err != nil {
			return code, stage, err
		}
		syncRepo.CreateWorkBranch = false // Keep repairs on the existing PR branch.
		if code, stage, err := h.pushWithSyncAndConflictResolution(ctx, &syncRepo, 0, runtime, codexDir, codexOpts, codexBasePrompt, agentsPath, cfg.ResponseMode, codexTargetLabel, agentStage); err != nil {
			return code, stage, err
		}
		repo.Changed = true
		repaired = true
	}
	return ExitPR, "pr", fmt.Errorf("pull request mergeability could not be confirmed after %d checks; conflicts may remain", maxChecks)
}
