package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

func TestClaimedIssuePreservesExistingCheckout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(dir, "pending.txt")
	if err := os.WriteFile(pending, []byte("unpublished work"), 0600); err != nil {
		t.Fatal(err)
	}
	h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if cmd.Name != "git" || !strings.Contains(strings.Join(cmd.Args, " "), "remote get-url origin") {
			t.Fatalf("preserved checkout was mutated: %+v", cmd)
		}
		return execx.Result{Stdout: "git@github.com:acme/repo.git\n"}, nil
	}))
	h.ResumeIssueWorkspace = true
	repo := repoWorkspace{URL: "https://github.com/acme/repo", Dir: dir, IssueClaimBranch: true}
	if err := h.cloneRepository(context.Background(), &repo, "agent00-issue-17", nil); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(pending); err != nil || string(body) != "unpublished work" {
		t.Fatal("unpublished work lost")
	}
	if !repo.CreateWorkBranch || repo.BaseBranch != "agent00-issue-17" {
		t.Fatal("resume lost branch targeting")
	}
}

func TestClaimedIssueResumesOnlyAcceptedPublication(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		ready           bool
		agentFails      bool
		wantPublish     bool
		wantExit        int
		wantCheckpoints []bool
	}{
		{name: "accepted work resumes publication", ready: true, wantPublish: true, wantExit: ExitGit},
		{name: "failed agent leaves partial work unpublished", agentFails: true, wantExit: ExitCodex, wantCheckpoints: []bool{false}},
		{name: "successful retry makes work publishable", wantPublish: true, wantExit: ExitGit, wantCheckpoints: []bool{false, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sampleConfig()
			cfg.GitHubIssue = &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, URL: "https://github.com/acme/repo/issues/17", Claim: &config.GitHubIssueClaim{ID: "claim", Branch: "agent00-issue-17", BaseBranch: "main", ExclusionLabel: "claimed", Attempt: 2}}
			publishing, agentStarted := false, false
			runner := issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				args := strings.Join(cmd.Args, " ")
				if cmd.Name == "codex" && !strings.Contains(args, "--help") {
					agentStarted = true
					if tc.agentFails {
						return execx.Result{Stdout: "Failure: Export incomplete.\nError details: Export waits for an upstream approval."}, nil
					}
					return execx.Result{}, nil
				}
				if cmd.Name == "git" {
					switch {
					case strings.Contains(args, "ls-remote --symref"):
						return execx.Result{Stdout: "ref: refs/heads/main\tHEAD\nbase\tHEAD\n"}, nil
					case strings.Contains(args, "branch --show-current"):
						return execx.Result{Stdout: "agent00-issue-17\n"}, nil
					case strings.Contains(args, "ls-remote --heads origin agent00-issue-17"):
						return execx.Result{Stdout: "existing\trefs/heads/agent00-issue-17\n"}, nil
					case strings.Contains(args, "status --porcelain"):
						return execx.Result{Stdout: "## agent00-issue-17\n M app.go\n"}, nil
					case strings.Contains(args, "add -A"):
						publishing = true
						return execx.Result{}, errors.New("stop at publication")
					}
				}
				return execx.Result{}, nil
			})
			h := New(runner)
			h.ResumeIssueWorkspace = true
			h.IssuePublicationReady = tc.ready
			var checkpoints []bool
			h.SaveIssuePublicationReady = func(ready bool) error {
				checkpoints = append(checkpoints, ready)
				return nil
			}
			h.Workspace = testWorkspaceManager("issue-publication-resume")
			h.TargetDirOK = func(string) bool { return true }
			result := h.Run(context.Background(), cfg)
			if publishing != tc.wantPublish || agentStarted == tc.ready || result.ExitCode != tc.wantExit {
				t.Fatalf("publishing=%t agent=%t result=%+v", publishing, agentStarted, result)
			}
			if !slices.Equal(checkpoints, tc.wantCheckpoints) {
				t.Fatalf("publication checkpoints=%v, want %v", checkpoints, tc.wantCheckpoints)
			}
		})
	}
}
