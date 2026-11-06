package app

import (
	"context"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// Test-only conveniences over production command builders and runners.

func (h Harness) verifyRemoteWriteAccess(ctx context.Context, repo repoWorkspace) error {
	return h.verifyRemoteWriteAccessOnRemote(ctx, repo, publishRemoteOrigin, "git")
}

func (h Harness) runCodex(
	ctx context.Context,
	runtime agentruntime.Runtime,
	targetDir string,
	prompt string,
	opts codexRunOptions,
	agentsPath string,
	responseMode string,
) error {
	_, err := h.runCodexCapture(ctx, runtime, targetDir, prompt, opts, agentsPath, responseMode, agentInvocationLogMetadata{})
	return err
}

func preflightCommands() []execx.Command {
	return preflightCommandsWithRuntime(agentruntime.Default())
}

func cloneCommand(cfg config.Config, repoDir string) execx.Command {
	if strings.TrimSpace(cfg.BaseBranch) == "" {
		return cloneRepoDefaultBranchCommand(cfg.RepoURL, repoDir)
	}
	return cloneRepoCommand(cfg.RepoURL, cfg.BaseBranch, repoDir)
}

func pushDryRunCommand(repoDir, branch string) execx.Command {
	return pushDryRunToRemoteCommand(repoDir, publishRemoteOrigin, branch)
}

func prCreateCommand(repoDir string, cfg config.Config, branch string) execx.Command {
	return prCreateWithOptionsCommand(repoDir, cfg, cfg.BaseBranch, branch, "")
}

func prCreateWithoutBaseCommand(repoDir string, cfg config.Config, branch string) execx.Command {
	return prCreateWithoutBaseWithOptionsCommand(repoDir, cfg, branch, "")
}

func prLookupByHeadCommand(repoDir, branch string) execx.Command {
	return prLookupByHeadWithRepoCommand(repoDir, branch, "")
}

func prLookupAnyByHeadCommand(repoDir, branch string) execx.Command {
	return prLookupAnyByHeadWithRepoCommand(repoDir, branch, "")
}

func withCompletionGatePrompt(prompt string) string {
	return withCompletionGatePromptForMode(prompt, true)
}
