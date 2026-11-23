package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

type issueBranchRunner func(context.Context, execx.Command) (execx.Result, error)

func (f issueBranchRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	return f(ctx, cmd)
}

func TestClaimedIssueRetryClonesAndFastForwardsReservedBranchBeforeAgent(t *testing.T) {
	t.Parallel()
	cfg := sampleConfig()
	cfg.GitHubIssue = &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, URL: "https://github.com/acme/repo/issues/17", Claim: &config.GitHubIssueClaim{ID: "claim", Branch: "agent00-issue-17", BaseBranch: "main", ExclusionLabel: "claimed", Attempt: 2}}
	const oldRequirement = "Run the old validation command."
	const newRequirement = "Run the new validation command."
	cloned, baseFetched, remoteFastForwarded, baseFastForwarded, agentStarted := false, false, false, false, false
	runner := issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		args := strings.Join(cmd.Args, " ")
		if cmd.Name == "git" {
			switch {
			case strings.Contains(args, "clone "):
				if !strings.Contains(args, "--branch agent00-issue-17") {
					t.Fatalf("wrong clone branch: %s", args)
				}
				cloned = true
				repoDir := cmd.Args[len(cmd.Args)-1]
				if err := os.MkdirAll(repoDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repoDir, backpressureFilename), []byte(oldRequirement), 0600); err != nil {
					t.Fatal(err)
				}
			case strings.Contains(args, "switch ") || strings.Contains(args, "checkout ") || strings.Contains(args, "branch -m"):
				t.Fatalf("claimed branch must not be created, reset, or renamed: %s", args)
			case strings.Contains(args, "fetch origin main:refs/remotes/origin/main"):
				baseFetched = true
			case args == "push --dry-run origin HEAD:refs/heads/agent00-issue-17":
				return execx.Result{Stderr: "! [rejected] HEAD -> agent00-issue-17 (non-fast-forward)"}, errors.New("push rejected")
			case args == "fetch origin refs/heads/agent00-issue-17" || args == "rev-list --max-count=1 FETCH_HEAD..HEAD":
				return execx.Result{}, nil
			case args == "merge --ff-only --no-overwrite-ignore FETCH_HEAD":
				remoteFastForwarded = true
			case strings.Contains(args, "status --porcelain"):
				return execx.Result{Stdout: "## agent00-issue-17\n"}, nil
			case strings.Contains(args, "rev-list --count refs/remotes/origin/main..HEAD"):
				return execx.Result{Stdout: "0\n"}, nil
			case strings.HasPrefix(args, "merge "):
				if !baseFetched {
					t.Error("fast-forward attempted before base fetch")
				}
				if !remoteFastForwarded {
					t.Error("base fast-forward attempted before syncing the reserved remote branch")
				}
				if args != "merge --ff-only --no-overwrite-ignore refs/remotes/origin/main" {
					t.Errorf("unexpected fast-forward command: %s", args)
				}
				if err := os.WriteFile(filepath.Join(cmd.Dir, backpressureFilename), []byte(newRequirement), 0600); err != nil {
					t.Fatal(err)
				}
				baseFastForwarded = true
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
			if !baseFastForwarded {
				t.Error("agent started before issue branch fast-forward")
			}
			if !strings.Contains(cmd.Stdin, newRequirement) || strings.Contains(cmd.Stdin, oldRequirement) {
				t.Error("agent prompt did not use updated backpressure requirements")
			}
			return execx.Result{Stdout: "Failure: Delivery blocked by unmerged #12.\nError details: Required changes are pending review."}, nil
		}
		return execx.Result{}, nil
	})
	h := New(runner)
	h.ResumeIssueWorkspace = true
	h.Workspace = testWorkspaceManager("issue-claim-branch")
	h.Workspace.Root = t.TempDir()
	h.TargetDirOK = func(string) bool { return true }
	result := h.Run(context.Background(), cfg)
	if !cloned || !baseFetched || !remoteFastForwarded || !baseFastForwarded || !agentStarted || result.ExitCode != ExitCodex ||
		result.Err == nil || !strings.Contains(result.Err.Error(), "reported failure") {
		t.Fatalf("clone/base fetched/remote fast-forward/base fast-forward/agent=%t/%t/%t/%t/%t result=%+v",
			cloned, baseFetched, remoteFastForwarded, baseFastForwarded, agentStarted, result)
	}
}

func TestClaimedIssueDefersNonFastForwardSyncUntilPublication(t *testing.T) {
	t.Parallel()
	const branch = "agent00-issue-17"
	for _, tc := range []struct {
		name  string
		dirty bool
	}{
		{name: "dirty worktree", dirty: true},
		{name: "divergent commits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probeCommands := []string{"push --dry-run origin HEAD:refs/heads/" + branch, "status --porcelain --branch"}
			status := "## " + branch + "\n"
			if tc.dirty {
				status += " M file.go\n"
			} else {
				probeCommands = append(probeCommands, "fetch origin refs/heads/"+branch, "rev-list --max-count=1 FETCH_HEAD..HEAD")
			}
			publishCommands := []string{"push -u origin " + branch, "fetch origin refs/heads/" + branch, "merge --no-edit --no-overwrite-ignore FETCH_HEAD", "push -u origin " + branch}
			wantCommands := append(probeCommands, publishCommands...)
			var commands []string
			pushes := 0
			h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				args := strings.Join(cmd.Args, " ")
				commands = append(commands, args)
				switch args {
				case "push --dry-run origin HEAD:refs/heads/" + branch:
					return execx.Result{Stderr: "(fetch first)"}, errors.New("push rejected")
				case "status --porcelain --branch":
					return execx.Result{Stdout: status}, nil
				case "rev-list --max-count=1 FETCH_HEAD..HEAD":
					return execx.Result{Stdout: "local-commit\n"}, nil
				case "push -u origin " + branch:
					pushes++
					if pushes == 1 {
						return execx.Result{Stderr: "(non-fast-forward)"}, errors.New("push rejected")
					}
				}
				return execx.Result{}, nil
			}))
			repo := repoWorkspace{Dir: t.TempDir(), Branch: branch, BaseBranch: "main", IssueClaimBranch: true, CreateWorkBranch: true}
			if err := h.preparePublishWorkflow(context.Background(), &repo); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(commands, probeCommands) {
				t.Fatalf("preflight commands = %v, want %v", commands, probeCommands)
			}
			if err := h.pushWithSync(context.Background(), &repo, 0, false); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(commands, wantCommands) {
				t.Fatalf("commands = %v, want %v", commands, wantCommands)
			}
			if repo.Branch != branch || repoPRHeadRef(repo) != branch || repo.BaseBranch != "main" || repo.BranchCollisionAttempts != 0 {
				t.Fatalf("issue branch targeting changed: %+v", repo)
			}
		})
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
		remoteOnly      bool
		localCommits    bool
		agentFails      bool
		wantPublish     bool
		wantExit        int
		wantCheckpoints []bool
	}{
		{name: "accepted work resumes publication", ready: true, wantPublish: true, wantExit: ExitGit},
		{name: "accepted local commits resume publication", ready: true, localCommits: true, wantPublish: true, wantExit: ExitGit},
		{name: "remote commits do not resume publication", ready: true, remoteOnly: true, agentFails: true, wantExit: ExitCodex, wantCheckpoints: []bool{false}},
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
						if tc.remoteOnly || tc.localCommits {
							return execx.Result{Stdout: "## agent00-issue-17\n"}, nil
						}
						return execx.Result{Stdout: "## agent00-issue-17\n M app.go\n"}, nil
					case args == "rev-list --max-count=1 FETCH_HEAD..HEAD":
						if tc.localCommits {
							return execx.Result{Stdout: "local-commit\n"}, nil
						}
						return execx.Result{}, nil
					case strings.Contains(args, "rev-list --count refs/remotes/origin/main..HEAD"):
						return execx.Result{Stdout: "1\n"}, nil
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
			if publishing != tc.wantPublish || agentStarted == (tc.ready && !tc.remoteOnly) || result.ExitCode != tc.wantExit {
				t.Fatalf("publishing=%t agent=%t result=%+v", publishing, agentStarted, result)
			}
			if !slices.Equal(checkpoints, tc.wantCheckpoints) {
				t.Fatalf("publication checkpoints=%v, want %v", checkpoints, tc.wantCheckpoints)
			}
		})
	}
}

func TestIssueConflictAgentRestoresPublicationReadiness(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			h := New(issueBranchRunner(func(context.Context, execx.Command) (execx.Result, error) {
				if failed {
					return execx.Result{Stdout: "Failure: Conflicts remain.\nError details: Unable to resolve the merge."}, nil
				}
				return execx.Result{Stdout: "Resolved merge conflicts."}, nil
			}))
			var checkpoints []bool
			h.SaveIssuePublicationReady = func(ready bool) error { checkpoints = append(checkpoints, ready); return nil }
			runtime := agentruntime.Default()
			_, err := h.runCodexCapture(context.Background(), runtime, t.TempDir(), "Resolve conflicts", codexRunOptions{}, "", "off", newAgentInvocationLogMetadata(runtime, "base_sync_conflict", 1, "repo", "repo", "."))
			want := []bool{false, true}
			if failed {
				want = []bool{false}
			}
			if (err != nil) != failed || !slices.Equal(checkpoints, want) {
				t.Fatalf("err=%v checkpoints=%v want=%v", err, checkpoints, want)
			}
		})
	}
}
