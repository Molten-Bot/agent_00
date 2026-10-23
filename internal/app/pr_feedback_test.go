package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
)

func TestRunFreeTextPRFeedbackUpdatesRequestedPR(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, selectedBranch, prompt, status string
		changed, drift                       bool
	}{
		{"selected head", "feature/api", "fix the comments in the review\n[High] api.go:1 - Missing validation", "## feature/api\n M api.go\n", true, false},
		{"PR URL", "main", "resolve the comments in this pr: https://github.com/acme/repo/pull/77", "## feature/api\n M api.go\n", true, false},
		{"already resolved", "feature/api", "fix the comments in the review", "## feature/api\n", false, false},
		{"branch drift", "feature/api", "fix the comments in the review", "## unrelated\n M api.go\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := sampleConfig()
			cfg.BaseBranch, cfg.Prompt = tc.selectedBranch, tc.prompt
			guid := "feedback-" + strings.ReplaceAll(tc.name, " ", "-")
			runDir := testRunDir(guid)
			repoDir := filepath.Join(runDir, "repo")
			targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
			prURL := "https://github.com/acme/repo/pull/77"
			selector := tc.selectedBranch
			if strings.Contains(tc.prompt, prURL) {
				selector = prURL
			}
			cloneCfg := cfg
			cloneCfg.BaseBranch = "feature/api"
			fake := &fakeRunner{t: t, exps: []expectedRun{
				{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
				{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
				{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
				{cmd: authCommand()},
				{cmd: ghCommand("", "pr", "view", selector, "--repo", "acme/repo", "--json", "url,state,headRefName,isCrossRepository"), res: execx.Result{Stdout: `{"url":"https://github.com/acme/repo/pull/77","state":"OPEN","headRefName":"feature/api"}`}},
				{cmd: cloneCommand(cloneCfg, repoDir)},
				{cmd: fetchBaseBranchCommand(repoDir, "feature/api")},
				{cmd: pushDryRunCommand(repoDir, "feature/api")},
				{cmd: headCommitSHACommand(repoDir), res: execx.Result{Stdout: "abc123\n"}},
				{cmd: codexCommand(targetDir, withAgentsPrompt(cfg.Prompt, filepath.Join(runDir, "AGENTS.md")))},
				{cmd: statusCommand(repoDir), res: execx.Result{Stdout: tc.status}},
			}}
			if tc.changed {
				fake.exps = append(fake.exps,
					expectedRun{cmd: addCommand(repoDir)},
					expectedRun{cmd: commitCommand(repoDir, cfg.CommitMessage)},
					expectedRun{cmd: pushCommand(repoDir, "feature/api")},
					expectedRun{cmd: prChecksCommand(repoDir, prURL)},
				)
			} else if !tc.drift {
				fake.exps = append(fake.exps, expectedRun{cmd: headCommitSHACommand(repoDir), res: execx.Result{Stdout: "abc123\n"}})
			}
			h := New(fake)
			h.Workspace = testWorkspaceManager(guid)
			h.TargetDirOK = func(string) bool { return true }
			res := h.Run(context.Background(), cfg)
			if tc.drift {
				if res.Err == nil || !strings.Contains(res.Err.Error(), "remain checked out") {
					t.Fatalf("Run() = %+v, want branch drift failure", res)
				}
			} else if res.Err != nil || res.PRURL != prURL || res.Branch != "feature/api" || res.NoChanges == tc.changed {
				t.Fatalf("Run() = %+v, want original PR and head branch", res)
			}
			if len(fake.exps) != 0 {
				t.Fatalf("unconsumed expectations: %+v", fake.exps)
			}
		})
	}
}

func TestPreparePRFeedbackTargetRejectsUnsafeTargets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, branch, prompt, response string
		lookupErr                      bool
	}{
		{"no target", "", "fix the comments in the review", "", false},
		{"default branch", "main", "fix the comments in the review", "", false},
		{"wrong repo", "feature/api", "fix PR comments https://github.com/other/repo/pull/77", "", false},
		{"multiple PRs", "feature/api", "fix PR comments https://github.com/acme/repo/pull/77 https://github.com/acme/repo/pull/78", "", false},
		{"closed PR", "feature/api", "fix PR comments", `{"url":"https://github.com/acme/repo/pull/77","state":"CLOSED","headRefName":"feature/api"}`, false},
		{"fork", "feature/api", "fix PR comments", `{"url":"https://github.com/acme/repo/pull/77","state":"OPEN","headRefName":"feature/api","isCrossRepository":true}`, false},
		{"wrong head", "feature/api", "fix PR comments", `{"url":"https://github.com/acme/repo/pull/77","state":"OPEN","headRefName":"other"}`, false},
		{"lookup failed", "feature/api", "fix PR comments", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := sampleConfig()
			cfg.BaseBranch, cfg.Prompt = tc.branch, tc.prompt
			fake := &fakeRunner{t: t}
			if tc.response != "" || tc.lookupErr {
				exp := expectedRun{cmd: ghCommand("", "pr", "view", tc.branch, "--repo", "acme/repo", "--json", "url,state,headRefName,isCrossRepository"), res: execx.Result{Stdout: tc.response}}
				if tc.lookupErr {
					exp.err = errors.New("PR not found")
				}
				fake.exps = []expectedRun{exp}
			}
			h := New(fake)
			if _, err := h.preparePRFeedbackTarget(context.Background(), &cfg); err == nil {
				t.Fatal("expected target error")
			}
			if len(fake.exps) != 0 {
				t.Fatal("target lookup not consumed")
			}
		})
	}
}

func TestFreeTextPRFeedbackDoesNotRetargetOtherTasks(t *testing.T) {
	t.Parallel()
	for _, cfg := range []config.Config{
		{Prompt: "Build a dashboard\n\nOriginal task: fix the comments in the review"},
		{Prompt: "Review this PR"},
		{Prompt: "Fix pull request creation in the harness"},
		{Prompt: "fix the comments in the review", Review: &config.ReviewConfig{PRNumber: 77}},
		{Prompt: "fix the comments in the review", LibraryTaskName: "resolve-pr-comments"},
		{RepoURL: config.DefaultRepositoryURL, Prompt: failurefollowup.RequiredPrompt + "\n\nOriginal task: fix PR comments"},
	} {
		if isFreeTextPRFeedbackRun(cfg) {
			t.Fatalf("unexpected free-text feedback classification: %+v", cfg)
		}
	}
}

func TestPRFeedbackDoesNotFallBackToAnotherBranchOrFork(t *testing.T) {
	t.Parallel()
	repo := repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo", RequiredBranch: "feature/api", RequireExistingPR: true}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: cloneRepoCommand(repo.URL, repo.RequiredBranch, repo.Dir), res: execx.Result{Stderr: "fatal: Remote branch feature/api not found in upstream origin"}, err: errors.New("exit status 128")},
	}}
	h := New(fake)
	if err := h.cloneRepository(context.Background(), &repo, repo.RequiredBranch, nil); err == nil || !strings.Contains(err.Error(), "refusing default-branch fallback") {
		t.Fatalf("cloneRepository() = %v, want no fallback", err)
	}
	if err := h.prepareForkFallbackPublishWorkflow(context.Background(), &repo); err == nil || !strings.Contains(err.Error(), "refusing fork fallback") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() = %v, want no fallback", err)
	}
	if len(fake.exps) != 0 {
		t.Fatal("clone expectation not consumed")
	}
}

func TestRunManualCodeReviewUsesOriginalPR(t *testing.T) {
	t.Parallel()
	for _, byURL := range []bool{false, true} {
		name := "head branch"
		if byURL {
			name = "PR URL"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := sampleConfig()
			cfg.LibraryTaskName = "code-review"
			cfg.BaseBranch = "feature/api"
			cfg.Prompt = "Review the pull request"
			selector := cfg.BaseBranch
			prURL := "https://github.com/acme/repo/pull/77"
			if byURL {
				cfg.BaseBranch = "main"
				cfg.Prompt += " " + prURL
				selector = "77"
			}
			guid := "manual-review-" + strings.ReplaceAll(name, " ", "-")
			runDir := testRunDir(guid)
			repoDir := filepath.Join(runDir, "repo")
			targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
			metadata := `{"number":77,"url":"https://github.com/acme/repo/pull/77","state":"OPEN","baseRefName":"main","headRefName":"feature/api"}`
			prompt := cfg.Prompt + "\n\n" + expectedPreparedReviewContext(cfg.RepoURL, metadata, expectedReviewDiscussion("[]", "[]", "[]"), "api.go | 1 +", "+validation")
			fake := &fakeRunner{t: t, exps: []expectedRun{
				{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
				{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
				{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
				{cmd: authCommand()},
				{cmd: cloneCommand(cfg, repoDir)},
				{cmd: prReviewMetadataCommand(repoDir, selector), res: execx.Result{Stdout: metadata}},
				{cmd: fetchRemoteBranchCommand(repoDir, "main")},
				{cmd: fetchPullRequestHeadCommand(repoDir, 77)},
				{cmd: prReviewIssueCommentsCommand(repoDir, 77), res: execx.Result{Stdout: "[]"}},
				{cmd: prReviewReviewCommentsCommand(repoDir, 77), res: execx.Result{Stdout: "[]"}},
				{cmd: prReviewReviewsCommand(repoDir, 77), res: execx.Result{Stdout: "[]"}},
				{cmd: reviewDiffStatCommand(repoDir, remoteTrackingRef("main"), pullRequestTrackingRef(77)), res: execx.Result{Stdout: "api.go | 1 +"}},
				{cmd: reviewDiffPatchCommand(repoDir, remoteTrackingRef("main"), pullRequestTrackingRef(77)), res: execx.Result{Stdout: "+validation"}},
				{cmd: codexCommandWithOptions(targetDir, withAgentsPrompt(prompt, filepath.Join(runDir, "AGENTS.md")), codexRunOptions{DisallowedTools: readOnlyReviewDisallowedTools})},
				{cmd: statusCommand(repoDir), res: execx.Result{Stdout: "## " + cfg.BaseBranch + "\n"}},
				{cmd: prReviewSummaryCommand(repoDir, "77", "acme/repo", reviewSummaryBodyPath(runDir, 77))},
			}}
			h := New(fake)
			h.Workspace = testWorkspaceManager(guid)
			h.TargetDirOK = func(string) bool { return true }
			res := h.Run(context.Background(), cfg)
			if res.Err != nil || !res.NoChanges || res.PRURL != prURL || res.Branch != "feature/api" {
				t.Fatalf("Run() = %+v, want read-only review on original PR", res)
			}
			if len(fake.exps) != 0 {
				t.Fatalf("unconsumed expectations: %+v", fake.exps)
			}
		})
	}
}
