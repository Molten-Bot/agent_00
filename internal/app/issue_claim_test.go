package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

type issueBranchRunner func(context.Context, execx.Command) (execx.Result, error)

func (f issueBranchRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	return f(ctx, cmd)
}

func TestClaimedIssueClonesReservedBranchWithoutCreatingAnother(t *testing.T) {
	t.Parallel()
	cfg := sampleConfig()
	cfg.GitHubIssue = &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, URL: "https://github.com/acme/repo/issues/17", Claim: &config.GitHubIssueClaim{ID: "claim", Branch: "agent00-issue-17", BaseBranch: "main", ExclusionLabel: "claimed", Attempt: 2}}
	cloned, baseFetched, agentStarted := false, false, false
	runner := issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		args := strings.Join(cmd.Args, " ")
		if cmd.Name == "git" {
			switch {
			case strings.Contains(args, "clone "):
				if !strings.Contains(args, "--branch agent00-issue-17") {
					t.Fatalf("wrong clone branch: %s", args)
				}
				cloned = true
			case strings.Contains(args, "switch ") || strings.Contains(args, "checkout ") || strings.Contains(args, "branch -m"):
				t.Fatalf("claimed branch must not be created, reset, or renamed: %s", args)
			case strings.Contains(args, "fetch origin main:refs/remotes/origin/main"):
				baseFetched = true
			case strings.Contains(args, "ls-remote --symref"):
				return execx.Result{Stdout: "ref: refs/heads/main\tHEAD\nbase\tHEAD\n"}, nil
			case strings.Contains(args, "branch --show-current"):
				return execx.Result{Stdout: "agent00-issue-17\n"}, nil
			case strings.Contains(args, "ls-remote --heads origin agent00-issue-17"):
				return execx.Result{Stdout: "existing\trefs/heads/agent00-issue-17\n"}, nil
			}
		}
		if cmd.Name == "codex" && !strings.Contains(args, "--help") {
			agentStarted = true
			return execx.Result{}, errors.New("stop after branch verification")
		}
		return execx.Result{}, nil
	})
	h := New(runner)
	h.Workspace = testWorkspaceManager("issue-claim-branch")
	h.TargetDirOK = func(string) bool { return true }
	result := h.Run(context.Background(), cfg)
	if !cloned || !baseFetched || !agentStarted || result.ExitCode != ExitCodex {
		t.Fatalf("clone/base/agent=%t/%t/%t result=%+v", cloned, baseFetched, agentStarted, result)
	}
}

func TestClaimedIssueRefusesCollisionRenameAndKeepsPRBase(t *testing.T) {
	t.Parallel()
	calls := 0
	h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		calls++
		args := strings.Join(cmd.Args, " ")
		if cmd.Name != "gh" || !strings.Contains(args, "pr create") || !strings.Contains(args, "--base main") || !strings.Contains(args, "--head agent00-issue-17") {
			t.Fatalf("unexpected PR command: %s %s", cmd.Name, args)
		}
		return execx.Result{Stdout: "https://github.com/acme/repo/pull/42"}, nil
	}))
	repo := repoWorkspace{URL: "https://github.com/acme/repo", Branch: "agent00-issue-17", BaseBranch: "main", IssueClaimBranch: true, CreateWorkBranch: true}
	if err := h.renameWorkBranchForRemoteCollision(context.Background(), &repo, "origin"); err == nil || calls != 0 {
		t.Fatal("reserved branch collision invoked rename")
	}
	if _, err := h.createPullRequestURL(context.Background(), repo, sampleConfig(), true, repo.Branch, ""); err != nil {
		t.Fatal(err)
	}
}
