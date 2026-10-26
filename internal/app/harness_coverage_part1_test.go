package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// This file targets low/zero-coverage functions in the first half of
// harness.go (roughly through countCommitsAheadOfBase): git sync / merge
// conflict detection, the direct-write vs fork-fallback publish workflow,
// PR URL lookup/creation helpers, non-default-branch enforcement, clone
// bootstrap edge cases, and the small git-status parsing helpers. Tests
// here call Harness methods directly with fakeRunner rather than going
// through a full Run() integration test, matching the style already used
// in coverage_targets_test.go. New helper names are prefixed h1cov to
// avoid collisions with helpers added by other agents working on other
// parts of this file concurrently.

func h1covRepo(dir string) *repoWorkspace {
	return &repoWorkspace{
		URL:    "git@github.com:acme/repo.git",
		Dir:    dir,
		RelDir: "repo",
		Branch: "agent00-build-api",
	}
}

// ---- isMergeConflictResult / hasUnmergedPaths ----

func TestH1CovIsMergeConflictResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		res  execx.Result
		err  error
		want bool
	}{
		{"nil err is never a conflict", execx.Result{Stdout: "CONFLICT"}, nil, false},
		{"empty text is not a conflict", execx.Result{}, errors.New(""), false},
		{"automatic merge failed marker", execx.Result{Stdout: "Automatic merge failed; fix conflicts and then commit the result."}, errors.New("exit status 1"), true},
		{"merge conflict marker", execx.Result{Stderr: "merge conflict detected"}, errors.New("exit status 1"), true},
		{"conflict paren marker", execx.Result{Stdout: "CONFLICT (content): Merge conflict in file.go"}, errors.New("exit status 1"), true},
		{"fix conflicts marker", execx.Result{Stdout: "fix conflicts and then commit"}, errors.New("exit status 1"), true},
		{"resolve index marker", execx.Result{Stderr: "error: you need to resolve your current index first"}, errors.New("exit status 128"), true},
		{"unmerged paths marker", execx.Result{Stderr: "error: you have unmerged paths."}, errors.New("exit status 128"), true},
		{"unrelated error is not a conflict", execx.Result{Stderr: "fatal: not a git repository"}, errors.New("exit status 128"), false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isMergeConflictResult(tc.res, tc.err); got != tc.want {
				t.Fatalf("isMergeConflictResult(%+v, %v) = %v, want %v", tc.res, tc.err, got, tc.want)
			}
		})
	}
}

func TestH1CovHasUnmergedPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{"empty status", "", false},
		{"clean status with header only", "## main...origin/main", false},
		{"short line ignored", "## main\nM\n", false},
		{"both unmerged U first", "## main\nUU file.go\n", true},
		{"both unmerged U second", "## main\nAU file.go\n", true},
		{"added-added conflict", "## main\nAA file.go\n", true},
		{"deleted-deleted conflict", "## main\nDD file.go\n", true},
		{"modified not unmerged", "## main\n M file.go\n", false},
		{"blank lines skipped", "## main\n\n\nUU file.go\n", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasUnmergedPaths(tc.status); got != tc.want {
				t.Fatalf("hasUnmergedPaths(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestH1CovSyncCommitMessages(t *testing.T) {
	t.Parallel()

	if got, want := baseSyncCommitMessage(""), "fix: sync with base"; got != want {
		t.Fatalf("baseSyncCommitMessage(\"\") = %q, want %q", got, want)
	}
	if got, want := baseSyncCommitMessage("  main  "), "fix: sync with main"; got != want {
		t.Fatalf("baseSyncCommitMessage(main) = %q, want %q", got, want)
	}
	if got, want := remoteBranchSyncCommitMessage(""), "fix: sync with remote branch"; got != want {
		t.Fatalf("remoteBranchSyncCommitMessage(\"\") = %q, want %q", got, want)
	}
	if got, want := remoteBranchSyncCommitMessage("origin/agent00-topic"), "fix: sync with agent00-topic"; got != want {
		t.Fatalf("remoteBranchSyncCommitMessage(origin/agent00-topic) = %q, want %q", got, want)
	}
}

// ---- GitHub CLI JSON parsing and marker detection ----

func TestH1CovParseGitHubViewerLogin(t *testing.T) {
	t.Parallel()

	if login, err := parseGitHubViewerLogin(`{"login":"octocat"}`); err != nil || login != "octocat" {
		t.Fatalf("parseGitHubViewerLogin() = (%q, %v), want (octocat, nil)", login, err)
	}
	if login, err := parseGitHubViewerLogin(`  {"login":"  spaced  "}  `); err != nil || login != "spaced" {
		t.Fatalf("parseGitHubViewerLogin() = (%q, %v), want (spaced, nil)", login, err)
	}
	if login, err := parseGitHubViewerLogin(`{"login":""}`); err != nil || login != "" {
		t.Fatalf("parseGitHubViewerLogin() = (%q, %v), want (\"\", nil)", login, err)
	}
	if _, err := parseGitHubViewerLogin(`not-json`); err == nil {
		t.Fatal("parseGitHubViewerLogin(not-json) err = nil, want decode error")
	}
}

func TestH1CovParseGitHubRepoVisibility(t *testing.T) {
	t.Parallel()

	view, err := parseGitHubRepoVisibility(`{"isPrivate":true,"nameWithOwner":"acme/repo"}`)
	if err != nil {
		t.Fatalf("parseGitHubRepoVisibility() err = %v, want nil", err)
	}
	if !view.IsPrivate || view.NameWithOwner != "acme/repo" {
		t.Fatalf("parseGitHubRepoVisibility() = %+v, want IsPrivate=true NameWithOwner=acme/repo", view)
	}
	if _, err := parseGitHubRepoVisibility(`{`); err == nil {
		t.Fatal("parseGitHubRepoVisibility(malformed) err = nil, want decode error")
	}
}

func TestH1CovIsForkAlreadyExistsError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		res  execx.Result
		err  error
		want bool
	}{
		{"nil err", execx.Result{Stdout: "already exists"}, nil, false},
		{"already exists", execx.Result{Stderr: "a repository with this name already exists"}, errors.New("exit status 1"), true},
		{"already have a fork", execx.Result{Stderr: "you already have a fork of this repo"}, errors.New("exit status 1"), true},
		{"already has a fork", execx.Result{Stdout: "octocat already has a fork of acme/repo"}, errors.New("exit status 1"), true},
		{"already forking", execx.Result{Stdout: "already forking this repository"}, errors.New("exit status 1"), true},
		{"already forked", execx.Result{Stdout: "repo already forked"}, errors.New("exit status 1"), true},
		{"unrelated error", execx.Result{Stderr: "rate limit exceeded"}, errors.New("exit status 1"), false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isForkAlreadyExistsError(tc.res, tc.err); got != tc.want {
				t.Fatalf("isForkAlreadyExistsError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestH1CovIsNoSuchRemoteError(t *testing.T) {
	t.Parallel()

	if isNoSuchRemoteError(execx.Result{Stderr: "error: No such remote 'fork'"}, nil) {
		t.Fatal("isNoSuchRemoteError() with nil err = true, want false")
	}
	if !isNoSuchRemoteError(execx.Result{Stderr: "error: No such remote 'fork'"}, errors.New("exit status 2")) {
		t.Fatal("isNoSuchRemoteError() = false, want true")
	}
	if isNoSuchRemoteError(execx.Result{Stderr: "fatal: unknown failure"}, errors.New("exit status 2")) {
		t.Fatal("isNoSuchRemoteError() = true, want false")
	}
}

// ---- repoPRHeadRef / promptPullRequestMatchesRepoBranch ----

func TestH1CovRepoPRHeadRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		repo repoWorkspace
		want string
	}{
		{
			name: "owner set wins",
			repo: repoWorkspace{Branch: "agent00-topic", PRHeadOwner: "octocat", PRHeadRef: "stale:old-branch"},
			want: "octocat:agent00-topic",
		},
		{
			name: "existing head ref with colon is re-derived from current branch",
			repo: repoWorkspace{Branch: "agent00-topic", PRHeadRef: "someowner:old-branch"},
			want: "someowner:agent00-topic",
		},
		{
			name: "head ref without colon is returned as-is",
			repo: repoWorkspace{Branch: "agent00-topic", PRHeadRef: "custom-head-ref"},
			want: "custom-head-ref",
		},
		{
			name: "falls back to normalized branch",
			repo: repoWorkspace{Branch: "origin/agent00-topic"},
			want: "agent00-topic",
		},
		{
			name: "empty everything",
			repo: repoWorkspace{},
			want: "",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := repoPRHeadRef(tc.repo); got != tc.want {
				t.Fatalf("repoPRHeadRef(%+v) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}

func TestH1CovPromptPullRequestMatchesRepoBranch(t *testing.T) {
	t.Parallel()

	if promptPullRequestMatchesRepoBranch(prStateView{}, repoWorkspace{Branch: "agent00-topic"}) {
		t.Fatal("empty head ref should never match")
	}
	if !promptPullRequestMatchesRepoBranch(prStateView{HeadRefName: "agent00-topic"}, repoWorkspace{Branch: "agent00-topic"}) {
		t.Fatal("matching branch should match")
	}
	if promptPullRequestMatchesRepoBranch(prStateView{HeadRefName: "agent00-other"}, repoWorkspace{Branch: "agent00-topic"}) {
		t.Fatal("mismatched branch should not match")
	}
	// repo.Branch empty falls back to branchNameFromHeadRef(repoPRHeadRef(repo)).
	fallback := repoWorkspace{PRHeadOwner: "octocat", PRHeadRef: "stale:ignored", Branch: "agent00-topic"}
	fallback.Branch = ""
	fallback.PRHeadRef = "octocat:agent00-topic"
	fallback.PRHeadOwner = ""
	if !promptPullRequestMatchesRepoBranch(prStateView{HeadRefName: "agent00-topic"}, fallback) {
		t.Fatal("fallback via repoPRHeadRef should match normalized branch")
	}
}

// ---- libraryTaskRequiresNonDefaultBranch (empty task name path only; a
// configured task name would load the on-disk library catalog, which is
// exercised by internal/library's own tests rather than here) ----

func TestH1CovLibraryTaskRequiresNonDefaultBranchEmptyTaskName(t *testing.T) {
	t.Parallel()

	got, err := libraryTaskRequiresNonDefaultBranch(config.Config{RequiresNonDefaultBranch: true})
	if err != nil || !got {
		t.Fatalf("libraryTaskRequiresNonDefaultBranch() = (%v, %v), want (true, nil)", got, err)
	}
	got, err = libraryTaskRequiresNonDefaultBranch(config.Config{RequiresNonDefaultBranch: false})
	if err != nil || got {
		t.Fatalf("libraryTaskRequiresNonDefaultBranch() = (%v, %v), want (false, nil)", got, err)
	}
}

// ---- ensureForkRemoteConfigured ----

func TestH1CovEnsureForkRemoteConfigured(t *testing.T) {
	t.Parallel()

	t.Run("set succeeds", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: gitRemoteSetURLCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git")},
		}}
		h := New(fake)
		if err := h.ensureForkRemoteConfigured(context.Background(), repo, "git@github.com:octocat/repo.git"); err != nil {
			t.Fatalf("ensureForkRemoteConfigured() err = %v, want nil", err)
		}
	})

	t.Run("set generic failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: gitRemoteSetURLCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git"),
				res: execx.Result{Stderr: "fatal: unexpected failure"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		err := h.ensureForkRemoteConfigured(context.Background(), repo, "git@github.com:octocat/repo.git")
		if err == nil || !strings.Contains(err.Error(), "set remote \"fork\" url for repo") {
			t.Fatalf("ensureForkRemoteConfigured() err = %v, want wrapped set-url error", err)
		}
	})

	t.Run("no such remote falls back to add", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: gitRemoteSetURLCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git"),
				res: execx.Result{Stderr: "error: No such remote 'fork'"},
				err: errors.New("exit status 2"),
			},
			{cmd: gitRemoteAddCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git")},
		}}
		h := New(fake)
		if err := h.ensureForkRemoteConfigured(context.Background(), repo, "git@github.com:octocat/repo.git"); err != nil {
			t.Fatalf("ensureForkRemoteConfigured() err = %v, want nil", err)
		}
	})

	t.Run("add failure after no such remote is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: gitRemoteSetURLCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git"),
				res: execx.Result{Stderr: "error: No such remote 'fork'"},
				err: errors.New("exit status 2"),
			},
			{
				cmd: gitRemoteAddCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git"),
				res: execx.Result{Stderr: "fatal: remote fork already exists"},
				err: errors.New("exit status 3"),
			},
		}}
		h := New(fake)
		err := h.ensureForkRemoteConfigured(context.Background(), repo, "git@github.com:octocat/repo.git")
		if err == nil || !strings.Contains(err.Error(), "add remote \"fork\" url for repo") {
			t.Fatalf("ensureForkRemoteConfigured() err = %v, want wrapped add error", err)
		}
	})
}

// ---- probeRemoteWriteAccessOnRemote defaulting ----

func TestH1CovProbeRemoteWriteAccessOnRemoteDefaultsEmptyArgs(t *testing.T) {
	t.Parallel()

	repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: pushDryRunToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch)},
	}}
	h := New(fake)
	probe, err := h.probeRemoteWriteAccessOnRemote(context.Background(), repo, "  ", "  ")
	if err != nil {
		t.Fatalf("probeRemoteWriteAccessOnRemote() err = %v, want nil", err)
	}
	if probe.NonFastForward {
		t.Fatalf("probe = %+v, want NonFastForward=false", probe)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}
}

// ---- preparePublishWorkflow guard clauses and non-remediable errors ----

func TestH1CovPreparePublishWorkflowNilRepo(t *testing.T) {
	t.Parallel()

	h := New(&fakeRunner{t: t})
	if err := h.preparePublishWorkflow(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
		t.Fatalf("preparePublishWorkflow(nil) err = %v, want workspace required error", err)
	}
}

func TestH1CovPreparePublishWorkflowEmptyBranch(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	h := New(&fakeRunner{t: t})
	err := h.preparePublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "branch is required") {
		t.Fatalf("preparePublishWorkflow() err = %v, want branch required error", err)
	}
}

func TestH1CovPreparePublishWorkflowNonRemediableErrorSkipsForkFallback(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo", Branch: "agent00-topic"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{
			cmd: pushDryRunToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch),
			res: execx.Result{Stderr: "fatal: unexpected server error"},
			err: errors.New("exit status 1"),
		},
	}}
	h := New(fake)
	err := h.preparePublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "verify remote write access for repo") {
		t.Fatalf("preparePublishWorkflow() err = %v, want wrapped probe error without fork fallback", err)
	}
	if !repo.WriteAccessChecked || repo.WriteAccessAllowed {
		t.Fatalf("write access flags = checked:%t allowed:%t, want checked=true allowed=false", repo.WriteAccessChecked, repo.WriteAccessAllowed)
	}
	if repo.WriteAccessErr == nil {
		t.Fatal("WriteAccessErr = nil, want recorded probe error")
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d (fork fallback should not have been attempted)", len(fake.exps))
	}
}

// ---- prepareForkFallbackPublishWorkflow edge cases ----

func TestH1CovPrepareForkFallbackPublishWorkflowNilRepo(t *testing.T) {
	t.Parallel()

	h := New(&fakeRunner{t: t})
	if err := h.prepareForkFallbackPublishWorkflow(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
		t.Fatalf("prepareForkFallbackPublishWorkflow(nil) err = %v, want workspace required error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowRequireExistingPRRefuses(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo", RequireExistingPR: true}
	h := New(&fakeRunner{t: t})
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if !errors.Is(err, ErrPRFeedbackTarget) {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want ErrPRFeedbackTarget", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowNonGitHubRepo(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "https://example.com/acme/repo.git", Dir: "/repo"}
	h := New(&fakeRunner{t: t})
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "non-GitHub repo") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want non-GitHub repo error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowRepoViewCommandFails(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{
			cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"),
			res: execx.Result{Stderr: "fatal: could not resolve repository"},
			err: errors.New("exit status 1"),
		},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "inspect upstream repository visibility") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want visibility inspection error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowRepoViewDecodeFails(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: "not-json"}},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "decode github repository visibility") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want visibility decode error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowPrivateRepo(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: `{"isPrivate":true,"nameWithOwner":"acme/repo"}`}},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "supports only public repositories") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want private-repo error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowViewerLookupFails(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: `{"isPrivate":false,"nameWithOwner":"acme/repo"}`}},
		{
			cmd: ghViewerLoginCommand(repo.Dir),
			res: execx.Result{Stderr: "fatal: not logged in"},
			err: errors.New("exit status 1"),
		},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "resolve authenticated github login") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want viewer login lookup error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowViewerLoginDecodeFails(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: `{"isPrivate":false,"nameWithOwner":"acme/repo"}`}},
		{cmd: ghViewerLoginCommand(repo.Dir), res: execx.Result{Stdout: "not-json"}},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "decode authenticated github login") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want login decode error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowViewerLoginEmpty(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: `{"isPrivate":false,"nameWithOwner":"acme/repo"}`}},
		{cmd: ghViewerLoginCommand(repo.Dir), res: execx.Result{Stdout: `{"login":""}`}},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "missing login") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want missing login error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowForkCreateHardFailure(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: `{"isPrivate":false,"nameWithOwner":"acme/repo"}`}},
		{cmd: ghViewerLoginCommand(repo.Dir), res: execx.Result{Stdout: `{"login":"octocat"}`}},
		{
			cmd: ghRepoForkCommand(repo.Dir, "acme/repo"),
			res: execx.Result{Stderr: "fatal: insufficient permission"},
			err: errors.New("exit status 1"),
		},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "create or reuse fork for acme/repo") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want fork creation error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowRemoteConfigureFails(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: `{"isPrivate":false,"nameWithOwner":"acme/repo"}`}},
		{cmd: ghViewerLoginCommand(repo.Dir), res: execx.Result{Stdout: `{"login":"octocat"}`}},
		{cmd: ghRepoForkCommand(repo.Dir, "acme/repo")},
		{
			cmd: gitRemoteSetURLCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git"),
			res: execx.Result{Stderr: "fatal: unexpected failure"},
			err: errors.New("exit status 1"),
		},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "set remote \"fork\" url for repo") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want remote configure error", err)
	}
}

func TestH1CovPrepareForkFallbackPublishWorkflowWriteAccessDeniedWithoutToken(t *testing.T) {
	t.Parallel()

	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: "/repo", Branch: "agent00-topic"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: ghRepoViewVisibilityCommand(repo.Dir, "acme/repo"), res: execx.Result{Stdout: `{"isPrivate":false,"nameWithOwner":"acme/repo"}`}},
		{cmd: ghViewerLoginCommand(repo.Dir), res: execx.Result{Stdout: `{"login":"octocat"}`}},
		{cmd: ghRepoForkCommand(repo.Dir, "acme/repo")},
		{cmd: gitRemoteSetURLCommand(repo.Dir, publishRemoteFork, "git@github.com:octocat/repo.git")},
		{
			cmd: pushDryRunToRemoteCommand(repo.Dir, publishRemoteFork, repo.Branch),
			res: execx.Result{Stderr: "remote: Write access to repository not granted."},
			err: errors.New("exit status 128"),
		},
	}}
	h := New(fake)
	err := h.prepareForkFallbackPublishWorkflow(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "verify remote write access for repo") {
		t.Fatalf("prepareForkFallbackPublishWorkflow() err = %v, want wrapped write access error", err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d (should not retry over HTTPS without a token)", len(fake.exps))
	}
}

// ---- lookupOpenPRURLByHead / lookupAnyPRURLByHead ----

func TestH1CovLookupOpenPRURLByHead(t *testing.T) {
	t.Parallel()

	t.Run("empty branch short-circuits", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		h := New(&fakeRunner{t: t})
		prURL, err := h.lookupOpenPRURLByHead(context.Background(), repo)
		if err != nil || prURL != "" {
			t.Fatalf("lookupOpenPRURLByHead() = (%q, %v), want (\"\", nil)", prURL, err)
		}
	})

	t.Run("remote branch check retryable error is wrapped as transient", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch),
				res: execx.Result{Stderr: "HTTP 502"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		_, err := h.lookupOpenPRURLByHead(context.Background(), repo)
		if !errors.Is(err, errTransientPRLookup) {
			t.Fatalf("lookupOpenPRURLByHead() err = %v, want errTransientPRLookup", err)
		}
	})

	t.Run("remote branch check non-retryable error propagates", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch),
				res: execx.Result{Stderr: "fatal: unknown failure"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		_, err := h.lookupOpenPRURLByHead(context.Background(), repo)
		if err == nil || errors.Is(err, errTransientPRLookup) {
			t.Fatalf("lookupOpenPRURLByHead() err = %v, want non-transient wrapped error", err)
		}
	})

	t.Run("remote branch missing returns empty", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: execx.Result{Stdout: ""}},
		}}
		h := New(fake)
		prURL, err := h.lookupOpenPRURLByHead(context.Background(), repo)
		if err != nil || prURL != "" {
			t.Fatalf("lookupOpenPRURLByHead() = (%q, %v), want (\"\", nil)", prURL, err)
		}
	})

	t.Run("lookup command failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: execx.Result{Stdout: "sha\trefs/heads/agent00-topic\n"}},
			{
				cmd: prLookupByHeadWithRepoCommand(repo.Dir, "agent00-topic", ""),
				res: execx.Result{Stderr: "fatal: unauthorized"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		_, err := h.lookupOpenPRURLByHead(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "lookup open pull request for repo") {
			t.Fatalf("lookupOpenPRURLByHead() err = %v, want wrapped lookup error", err)
		}
	})

	t.Run("url parsed from stdout", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: execx.Result{Stdout: "sha\trefs/heads/agent00-topic\n"}},
			{
				cmd: prLookupByHeadWithRepoCommand(repo.Dir, "agent00-topic", ""),
				res: execx.Result{Stdout: `[{"url":"https://github.com/acme/repo/pull/7"}]`},
			},
		}}
		h := New(fake)
		prURL, err := h.lookupOpenPRURLByHead(context.Background(), repo)
		if err != nil || prURL != "https://github.com/acme/repo/pull/7" {
			t.Fatalf("lookupOpenPRURLByHead() = (%q, %v), want pull/7 URL", prURL, err)
		}
	})

	t.Run("url parsed from stderr when stdout has none", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: execx.Result{Stdout: "sha\trefs/heads/agent00-topic\n"}},
			{
				cmd: prLookupByHeadWithRepoCommand(repo.Dir, "agent00-topic", ""),
				res: execx.Result{Stdout: "[]", Stderr: "see https://github.com/acme/repo/pull/9 for details"},
			},
		}}
		h := New(fake)
		prURL, err := h.lookupOpenPRURLByHead(context.Background(), repo)
		if err != nil || prURL != "https://github.com/acme/repo/pull/9" {
			t.Fatalf("lookupOpenPRURLByHead() = (%q, %v), want pull/9 URL", prURL, err)
		}
	})
}

func TestH1CovLookupAnyPRURLByHead(t *testing.T) {
	t.Parallel()

	t.Run("empty branch short-circuits", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		h := New(&fakeRunner{t: t})
		prURL, err := h.lookupAnyPRURLByHead(context.Background(), repo)
		if err != nil || prURL != "" {
			t.Fatalf("lookupAnyPRURLByHead() = (%q, %v), want (\"\", nil)", prURL, err)
		}
	})

	t.Run("lookup command failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: prLookupAnyByHeadWithRepoCommand(repo.Dir, "agent00-topic", ""),
				res: execx.Result{Stderr: "fatal: unauthorized"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		_, err := h.lookupAnyPRURLByHead(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "lookup any pull request for repo") {
			t.Fatalf("lookupAnyPRURLByHead() err = %v, want wrapped lookup error", err)
		}
	})

	t.Run("url parsed from stdout", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: prLookupAnyByHeadWithRepoCommand(repo.Dir, "agent00-topic", ""),
				res: execx.Result{Stdout: `[{"url":"https://github.com/acme/repo/pull/11"}]`},
			},
		}}
		h := New(fake)
		prURL, err := h.lookupAnyPRURLByHead(context.Background(), repo)
		if err != nil || prURL != "https://github.com/acme/repo/pull/11" {
			t.Fatalf("lookupAnyPRURLByHead() = (%q, %v), want pull/11 URL", prURL, err)
		}
	})
}

// ---- createPullRequestURL ----

func TestH1CovCreatePullRequestURL(t *testing.T) {
	t.Parallel()

	t.Run("existing PR reuse on already-exists failure", func(t *testing.T) {
		t.Parallel()
		cfg := sampleConfig()
		repo := repoWorkspace{Dir: "/repo", URL: cfg.RepoURL, Branch: "agent00-topic", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: prCreateWithOptionsCommand(repo.Dir, cfg, repo.BaseBranch, "agent00-topic", ""),
				res: execx.Result{Stderr: "a pull request for branch \"agent00-topic\" into branch \"main\" already exists: https://github.com/acme/repo/pull/3"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		prURL, err := h.createPullRequestURL(context.Background(), repo, cfg, true, "agent00-topic", "")
		if err != nil || prURL != "https://github.com/acme/repo/pull/3" {
			t.Fatalf("createPullRequestURL() = (%q, %v), want pull/3 reuse", prURL, err)
		}
	})

	t.Run("transient failure falls back to lookup success", func(t *testing.T) {
		t.Parallel()
		cfg := sampleConfig()
		repo := repoWorkspace{Dir: "/repo", URL: cfg.RepoURL, Branch: "agent00-topic", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: prCreateWithOptionsCommand(repo.Dir, cfg, repo.BaseBranch, "agent00-topic", ""),
				res: execx.Result{Stderr: "HTTP 500"},
				err: errors.New("exit status 1"),
			},
			{cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: execx.Result{Stdout: "sha\trefs/heads/agent00-topic\n"}},
			{
				cmd: prLookupByHeadWithRepoCommand(repo.Dir, "agent00-topic", ""),
				res: execx.Result{Stdout: `[{"url":"https://github.com/acme/repo/pull/4"}]`},
			},
		}}
		h := New(fake)
		prURL, err := h.createPullRequestURL(context.Background(), repo, cfg, true, "agent00-topic", "")
		if err != nil || prURL != "https://github.com/acme/repo/pull/4" {
			t.Fatalf("createPullRequestURL() = (%q, %v), want pull/4 via lookup fallback", prURL, err)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("permission denied failure is not retried", func(t *testing.T) {
		t.Parallel()
		cfg := sampleConfig()
		repo := repoWorkspace{Dir: "/repo", URL: cfg.RepoURL, Branch: "agent00-topic", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: prCreateWithOptionsCommand(repo.Dir, cfg, repo.BaseBranch, "agent00-topic", ""),
				res: execx.Result{Stderr: "pulls.createPullRequest(): You do not have correct permissions to create a pull request"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		_, err := h.createPullRequestURL(context.Background(), repo, cfg, true, "agent00-topic", "")
		if !errors.Is(err, errPRCreatePermissionDenied) {
			t.Fatalf("createPullRequestURL() err = %v, want errPRCreatePermissionDenied", err)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d, want no retry for permission denied", len(fake.exps))
		}
	})

	t.Run("success extracts URL from stdout without createWorkBranch", func(t *testing.T) {
		t.Parallel()
		cfg := sampleConfig()
		repo := repoWorkspace{Dir: "/repo", URL: cfg.RepoURL, Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: prCreateWithoutBaseWithOptionsCommand(repo.Dir, cfg, "agent00-topic", "acme/repo"),
				res: execx.Result{Stdout: "https://github.com/acme/repo/pull/5\n"},
			},
		}}
		h := New(fake)
		prURL, err := h.createPullRequestURL(context.Background(), repo, cfg, false, "agent00-topic", "acme/repo")
		if err != nil || prURL != "https://github.com/acme/repo/pull/5" {
			t.Fatalf("createPullRequestURL() = (%q, %v), want pull/5", prURL, err)
		}
	})

	t.Run("success with no URL in output falls back to lookup", func(t *testing.T) {
		t.Parallel()
		cfg := sampleConfig()
		repo := repoWorkspace{Dir: "/repo", URL: cfg.RepoURL, Branch: "agent00-topic", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prCreateWithOptionsCommand(repo.Dir, cfg, repo.BaseBranch, "agent00-topic", "")},
			{cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: execx.Result{Stdout: "sha\trefs/heads/agent00-topic\n"}},
			{
				cmd: prLookupByHeadWithRepoCommand(repo.Dir, "agent00-topic", ""),
				res: execx.Result{Stdout: `[{"url":"https://github.com/acme/repo/pull/6"}]`},
			},
		}}
		h := New(fake)
		prURL, err := h.createPullRequestURL(context.Background(), repo, cfg, true, "agent00-topic", "")
		if err != nil || prURL != "https://github.com/acme/repo/pull/6" {
			t.Fatalf("createPullRequestURL() = (%q, %v), want pull/6 via post-create lookup", prURL, err)
		}
	})

	t.Run("success with no URL anywhere is an error", func(t *testing.T) {
		t.Parallel()
		cfg := sampleConfig()
		repo := repoWorkspace{Dir: "/repo", URL: cfg.RepoURL, Branch: "agent00-topic", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prCreateWithOptionsCommand(repo.Dir, cfg, repo.BaseBranch, "agent00-topic", "")},
			{cmd: remoteBranchExistsOnRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: execx.Result{Stdout: ""}},
		}}
		h := New(fake)
		_, err := h.createPullRequestURL(context.Background(), repo, cfg, true, "agent00-topic", "")
		if err == nil || !strings.Contains(err.Error(), "did not return a PR URL") {
			t.Fatalf("createPullRequestURL() err = %v, want missing-URL error", err)
		}
	})
}

// ---- populateNoChangePRURLs ----

func TestH1CovPopulateNoChangePRURLs(t *testing.T) {
	t.Parallel()

	t.Run("already has PRURL with RequireExistingPR is skipped", func(t *testing.T) {
		t.Parallel()
		repos := []repoWorkspace{{Dir: "/repo", RequireExistingPR: true, PRURL: "https://github.com/acme/repo/pull/1"}}
		h := New(&fakeRunner{t: t})
		if err := h.populateNoChangePRURLs(context.Background(), repos, "", false); err != nil {
			t.Fatalf("populateNoChangePRURLs() err = %v, want nil", err)
		}
		if repos[0].PRURL != "https://github.com/acme/repo/pull/1" {
			t.Fatalf("PRURL = %q, want unchanged", repos[0].PRURL)
		}
	})

	t.Run("lookup failure without requirePRURL is tolerated", func(t *testing.T) {
		t.Parallel()
		repos := []repoWorkspace{{Dir: "/repo", Branch: "agent00-topic"}}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: remoteBranchExistsOnRemoteCommand(repos[0].Dir, publishRemoteOrigin, repos[0].Branch),
				res: execx.Result{Stderr: "fatal: unauthorized"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		if err := h.populateNoChangePRURLs(context.Background(), repos, "", false); err != nil {
			t.Fatalf("populateNoChangePRURLs() err = %v, want nil (tolerated)", err)
		}
		if repos[0].PRURL != "" {
			t.Fatalf("PRURL = %q, want empty", repos[0].PRURL)
		}
	})

	t.Run("lookup failure with requirePRURL fails", func(t *testing.T) {
		t.Parallel()
		repos := []repoWorkspace{{Dir: "/repo", Branch: "agent00-topic"}}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: remoteBranchExistsOnRemoteCommand(repos[0].Dir, publishRemoteOrigin, repos[0].Branch),
				res: execx.Result{Stderr: "fatal: unauthorized"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		err := h.populateNoChangePRURLs(context.Background(), repos, "", true)
		if err == nil || !strings.Contains(err.Error(), "verify existing pull request") {
			t.Fatalf("populateNoChangePRURLs() err = %v, want verify-existing error", err)
		}
	})

	t.Run("open lookup empty falls back to any lookup", func(t *testing.T) {
		t.Parallel()
		repos := []repoWorkspace{{Dir: "/repo", Branch: "agent00-topic"}}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repos[0].Dir, publishRemoteOrigin, repos[0].Branch), res: execx.Result{Stdout: ""}},
			{
				cmd: prLookupAnyByHeadWithRepoCommand(repos[0].Dir, "agent00-topic", ""),
				res: execx.Result{Stdout: `[{"url":"https://github.com/acme/repo/pull/2"}]`},
			},
		}}
		h := New(fake)
		if err := h.populateNoChangePRURLs(context.Background(), repos, "", false); err != nil {
			t.Fatalf("populateNoChangePRURLs() err = %v, want nil", err)
		}
		if repos[0].PRURL != "https://github.com/acme/repo/pull/2" {
			t.Fatalf("PRURL = %q, want pull/2 from any-lookup fallback", repos[0].PRURL)
		}
	})

	t.Run("prompt PR URL used when head matches", func(t *testing.T) {
		t.Parallel()
		promptPRURL := "https://github.com/acme/repo/pull/8"
		repos := []repoWorkspace{{Dir: "/repo", Branch: "agent00-topic"}}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repos[0].Dir, publishRemoteOrigin, repos[0].Branch), res: execx.Result{Stdout: ""}},
			{cmd: prLookupAnyByHeadWithRepoCommand(repos[0].Dir, "agent00-topic", ""), res: execx.Result{Stdout: "[]"}},
			{cmd: prStateViewCommand(repos[0].Dir, promptPRURL), res: execx.Result{Stdout: `{"url":"` + promptPRURL + `","state":"open","headRefName":"agent00-topic"}`}},
		}}
		h := New(fake)
		prompt := "Original task prompt mentions " + promptPRURL
		if err := h.populateNoChangePRURLs(context.Background(), repos, prompt, false); err != nil {
			t.Fatalf("populateNoChangePRURLs() err = %v, want nil", err)
		}
		if repos[0].PRURL != promptPRURL {
			t.Fatalf("PRURL = %q, want %q", repos[0].PRURL, promptPRURL)
		}
	})

	t.Run("prompt PR URL branch mismatch with requirePRURL fails", func(t *testing.T) {
		t.Parallel()
		promptPRURL := "https://github.com/acme/repo/pull/8"
		repos := []repoWorkspace{{Dir: "/repo", Branch: "agent00-topic"}}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repos[0].Dir, publishRemoteOrigin, repos[0].Branch), res: execx.Result{Stdout: ""}},
			{cmd: prLookupAnyByHeadWithRepoCommand(repos[0].Dir, "agent00-topic", ""), res: execx.Result{Stdout: "[]"}},
			{cmd: prStateViewCommand(repos[0].Dir, promptPRURL), res: execx.Result{Stdout: `{"url":"` + promptPRURL + `","state":"open","headRefName":"agent00-other"}`}},
		}}
		h := New(fake)
		prompt := "Original task prompt mentions " + promptPRURL
		err := h.populateNoChangePRURLs(context.Background(), repos, prompt, true)
		if err == nil || !strings.Contains(err.Error(), "did not match") {
			t.Fatalf("populateNoChangePRURLs() err = %v, want branch mismatch error", err)
		}
	})

	t.Run("no PR found anywhere with requirePRURL fails", func(t *testing.T) {
		t.Parallel()
		repos := []repoWorkspace{{Dir: "/repo", Branch: "agent00-topic"}}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: remoteBranchExistsOnRemoteCommand(repos[0].Dir, publishRemoteOrigin, repos[0].Branch), res: execx.Result{Stdout: ""}},
			{cmd: prLookupAnyByHeadWithRepoCommand(repos[0].Dir, "agent00-topic", ""), res: execx.Result{Stdout: "[]"}},
		}}
		h := New(fake)
		err := h.populateNoChangePRURLs(context.Background(), repos, "", true)
		if err == nil || !strings.Contains(err.Error(), "no pull request found") {
			t.Fatalf("populateNoChangePRURLs() err = %v, want no-PR-found error", err)
		}
	})
}

// ---- validateEnforcedNonDefaultBranchCheckouts / Checkout ----

func TestH1CovValidateEnforcedNonDefaultBranchCheckouts(t *testing.T) {
	t.Parallel()

	t.Run("not required across repos is a no-op", func(t *testing.T) {
		t.Parallel()
		repos := []repoWorkspace{{Dir: "/repo-a"}, {Dir: "/repo-b"}}
		h := New(&fakeRunner{t: t})
		if err := h.validateEnforcedNonDefaultBranchCheckouts(context.Background(), repos); err != nil {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckouts() err = %v, want nil", err)
		}
	})

	t.Run("second repo failure stops the batch", func(t *testing.T) {
		t.Parallel()
		repos := []repoWorkspace{
			{Dir: "/repo-a"},
			{Dir: "/repo-b", RequiresNonDefaultBranch: true, RequiredBranch: "main", RequiredBranchTask: "feature-task", Branch: "main"},
		}
		h := New(&fakeRunner{t: t})
		err := h.validateEnforcedNonDefaultBranchCheckouts(context.Background(), repos)
		if err == nil || !strings.Contains(err.Error(), "protected default branch name") {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckouts() err = %v, want protected branch error", err)
		}
	})
}

func TestH1CovValidateEnforcedNonDefaultBranchCheckout(t *testing.T) {
	t.Parallel()

	t.Run("not required is a no-op", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		if err := h.validateEnforcedNonDefaultBranchCheckout(context.Background(), repoWorkspace{Dir: "/repo"}); err != nil {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckout() err = %v, want nil", err)
		}
	})

	t.Run("protected branch name is rejected", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RequiresNonDefaultBranch: true, RequiredBranch: "master", RequiredBranchTask: "feature-task", Branch: "master"}
		h := New(&fakeRunner{t: t})
		err := h.validateEnforcedNonDefaultBranchCheckout(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "protected default branch name") {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckout() err = %v, want protected branch error", err)
		}
	})

	t.Run("publish branch mismatch is rejected", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task", Branch: "agent00-other"}
		h := New(&fakeRunner{t: t})
		err := h.validateEnforcedNonDefaultBranchCheckout(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "remain pinned") {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckout() err = %v, want pinned-branch error", err)
		}
	})

	t.Run("current branch command failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task", Branch: "release/2026"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stderr: "fatal: not a git repository"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		err := h.validateEnforcedNonDefaultBranchCheckout(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "verify checked out branch for repo") {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckout() err = %v, want wrapped command error", err)
		}
	})

	t.Run("detached HEAD is rejected", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task", Branch: "release/2026"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stdout: "\n"}},
		}}
		h := New(fake)
		err := h.validateEnforcedNonDefaultBranchCheckout(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "left HEAD detached") {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckout() err = %v, want detached-HEAD error", err)
		}
	})

	t.Run("checked out branch mismatch is rejected", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task", Branch: "release/2026"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stdout: "some-other-branch\n"}},
		}}
		h := New(fake)
		err := h.validateEnforcedNonDefaultBranchCheckout(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "current branch is") {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckout() err = %v, want branch-mismatch error", err)
		}
	})

	t.Run("matching checkout succeeds", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task", Branch: "release/2026"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stdout: "release/2026\n"}},
		}}
		h := New(fake)
		if err := h.validateEnforcedNonDefaultBranchCheckout(context.Background(), repo); err != nil {
			t.Fatalf("validateEnforcedNonDefaultBranchCheckout() err = %v, want nil", err)
		}
	})
}

// ---- renameGeneratedWorkBranch ----

func TestH1CovRenameGeneratedWorkBranch(t *testing.T) {
	t.Parallel()

	t.Run("nil repo", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		if err := h.renameGeneratedWorkBranch(context.Background(), nil, "origin", "reason"); err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
			t.Fatalf("renameGeneratedWorkBranch(nil) err = %v, want workspace required error", err)
		}
	})

	t.Run("empty branch", func(t *testing.T) {
		t.Parallel()
		repo := h1covRepo("/repo")
		repo.Branch = ""
		h := New(&fakeRunner{t: t})
		if err := h.renameGeneratedWorkBranch(context.Background(), repo, "origin", "reason"); err == nil || !strings.Contains(err.Error(), "work branch is required") {
			t.Fatalf("renameGeneratedWorkBranch() err = %v, want branch-required error", err)
		}
	})

	t.Run("branch move failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := h1covRepo("/repo")
		newBranch := "agent00-build-api-retry"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: branchMoveCommand(repo.Dir, newBranch),
				res: execx.Result{Stderr: "fatal: a branch named 'agent00-build-api-retry' already exists"},
				err: errors.New("exit status 128"),
			},
		}}
		h := New(fake)
		err := h.renameGeneratedWorkBranch(context.Background(), repo, "origin", "")
		if err == nil || !strings.Contains(err.Error(), "rename work branch") {
			t.Fatalf("renameGeneratedWorkBranch() err = %v, want wrapped rename error", err)
		}
		if repo.Branch != "agent00-build-api" {
			t.Fatalf("Branch = %q, want unchanged after failed rename", repo.Branch)
		}
	})

	t.Run("success updates branch and publish head ref", func(t *testing.T) {
		t.Parallel()
		repo := h1covRepo("/repo")
		repo.PRHeadOwner = "octocat"
		newBranch := "agent00-build-api-retry"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: branchMoveCommand(repo.Dir, newBranch)},
		}}
		h := New(fake)
		if err := h.renameGeneratedWorkBranch(context.Background(), repo, "origin", "closed_pr"); err != nil {
			t.Fatalf("renameGeneratedWorkBranch() err = %v, want nil", err)
		}
		if repo.Branch != newBranch {
			t.Fatalf("Branch = %q, want %q", repo.Branch, newBranch)
		}
		if repo.BranchCollisionAttempts != 1 {
			t.Fatalf("BranchCollisionAttempts = %d, want 1", repo.BranchCollisionAttempts)
		}
		if want := "octocat:" + newBranch; repo.PRHeadRef != want {
			t.Fatalf("PRHeadRef = %q, want %q", repo.PRHeadRef, want)
		}
	})
}

// ---- refreshRepoChangeStateAfterNoOpCommit / updateRepoBranchFromStatus ----

func TestH1CovRefreshRepoChangeStateAfterNoOpCommit(t *testing.T) {
	t.Parallel()

	t.Run("nil repo returns false", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		noChanges, err := h.refreshRepoChangeStateAfterNoOpCommit(context.Background(), nil, execx.Result{}, errors.New("boom"))
		if err != nil || noChanges {
			t.Fatalf("refreshRepoChangeStateAfterNoOpCommit(nil) = (%v, %v), want (false, nil)", noChanges, err)
		}
	})

	t.Run("non nothing-to-commit error returns false", func(t *testing.T) {
		t.Parallel()
		repo := h1covRepo("/repo")
		h := New(&fakeRunner{t: t})
		noChanges, err := h.refreshRepoChangeStateAfterNoOpCommit(context.Background(), repo, execx.Result{}, errors.New("disk full"))
		if err != nil || noChanges {
			t.Fatalf("refreshRepoChangeStateAfterNoOpCommit() = (%v, %v), want (false, nil)", noChanges, err)
		}
	})

	t.Run("status command failure propagates", func(t *testing.T) {
		t.Parallel()
		repo := h1covRepo("/repo")
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: statusCommand(repo.Dir), err: errors.New("status failed")},
		}}
		h := New(fake)
		_, err := h.refreshRepoChangeStateAfterNoOpCommit(context.Background(), repo, execx.Result{}, errors.New("nothing to commit, working tree clean"))
		if err == nil || err.Error() != "status failed" {
			t.Fatalf("refreshRepoChangeStateAfterNoOpCommit() err = %v, want status failed", err)
		}
	})

	t.Run("still has pending changes after no-op commit", func(t *testing.T) {
		t.Parallel()
		repo := h1covRepo("/repo")
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: statusCommand(repo.Dir), res: execx.Result{Stdout: "## agent00-build-api\n M file.go\n"}},
		}}
		h := New(fake)
		noChanges, err := h.refreshRepoChangeStateAfterNoOpCommit(context.Background(), repo, execx.Result{}, errors.New("nothing to commit, working tree clean"))
		if err != nil {
			t.Fatalf("refreshRepoChangeStateAfterNoOpCommit() err = %v, want nil", err)
		}
		if noChanges {
			t.Fatal("refreshRepoChangeStateAfterNoOpCommit() noChanges = true, want false (tracked changes remain)")
		}
		if !repo.Changed {
			t.Fatal("repo.Changed = false, want true")
		}
	})

	t.Run("no changes after add reports noChanges true", func(t *testing.T) {
		t.Parallel()
		repo := h1covRepo("/repo")
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: statusCommand(repo.Dir), res: execx.Result{Stdout: "## agent00-build-api\n"}},
		}}
		h := New(fake)
		noChanges, err := h.refreshRepoChangeStateAfterNoOpCommit(context.Background(), repo, execx.Result{}, errors.New("nothing to commit, working tree clean"))
		if err != nil || !noChanges {
			t.Fatalf("refreshRepoChangeStateAfterNoOpCommit() = (%v, %v), want (true, nil)", noChanges, err)
		}
		if repo.Changed {
			t.Fatal("repo.Changed = true, want false")
		}
	})
}

func TestH1CovUpdateRepoBranchFromStatus(t *testing.T) {
	t.Parallel()

	t.Run("nil repo errors", func(t *testing.T) {
		t.Parallel()
		if err := updateRepoBranchFromStatus(nil, "## main"); err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
			t.Fatalf("updateRepoBranchFromStatus(nil) err = %v, want workspace required error", err)
		}
	})

	t.Run("PR feedback branch mismatch errors", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{RequireExistingPR: true, RequiredBranch: "agent00-fix"}
		err := updateRepoBranchFromStatus(repo, "## agent00-other...origin/agent00-other")
		if !errors.Is(err, ErrPRFeedbackTarget) {
			t.Fatalf("updateRepoBranchFromStatus() err = %v, want ErrPRFeedbackTarget", err)
		}
	})

	t.Run("required branch empty observed errors", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task"}
		err := updateRepoBranchFromStatus(repo, "no branch header here")
		if err == nil || !strings.Contains(err.Error(), "git status did not report a branch") {
			t.Fatalf("updateRepoBranchFromStatus() err = %v, want empty-branch error", err)
		}
	})

	t.Run("required branch mismatch errors", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task"}
		err := updateRepoBranchFromStatus(repo, "## agent00-other...origin/agent00-other")
		if err == nil || !strings.Contains(err.Error(), "git status reported") {
			t.Fatalf("updateRepoBranchFromStatus() err = %v, want mismatch error", err)
		}
	})

	t.Run("required branch match pins branch and publish head ref", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task"}
		if err := updateRepoBranchFromStatus(repo, "## release/2026...origin/release/2026"); err != nil {
			t.Fatalf("updateRepoBranchFromStatus() err = %v, want nil", err)
		}
		if repo.Branch != "release/2026" || repo.PRHeadRef != "release/2026" {
			t.Fatalf("repo = %+v, want Branch=release/2026 PRHeadRef=release/2026", repo)
		}
	})

	t.Run("non-required path prefers observed branch over existing", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Branch: "agent00-stale"}
		if err := updateRepoBranchFromStatus(repo, "## agent00-fresh...origin/agent00-fresh"); err != nil {
			t.Fatalf("updateRepoBranchFromStatus() err = %v, want nil", err)
		}
		if repo.Branch != "agent00-fresh" {
			t.Fatalf("Branch = %q, want agent00-fresh", repo.Branch)
		}
	})

	t.Run("non-required path falls back to existing branch when status has none", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Branch: "agent00-existing"}
		if err := updateRepoBranchFromStatus(repo, "no branch header"); err != nil {
			t.Fatalf("updateRepoBranchFromStatus() err = %v, want nil", err)
		}
		if repo.Branch != "agent00-existing" {
			t.Fatalf("Branch = %q, want agent00-existing (fallback preserved)", repo.Branch)
		}
	})
}

// ---- repoHasPendingChanges / repoHasPullRequestDelta / repoHeadChangedSinceBaseline / currentHead / countCommitsAheadOfBase ----

func TestH1CovRepoHasPendingChanges(t *testing.T) {
	t.Parallel()

	t.Run("tracked worktree changes short-circuit true", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		changed, err := h.repoHasPendingChanges(context.Background(), repoWorkspace{Dir: "/repo"}, "## main\n M file.go\n")
		if err != nil || !changed {
			t.Fatalf("repoHasPendingChanges() = (%v, %v), want (true, nil)", changed, err)
		}
	})

	t.Run("no branch header with no tracked changes is false", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		changed, err := h.repoHasPendingChanges(context.Background(), repoWorkspace{Dir: "/repo"}, "")
		if err != nil || changed {
			t.Fatalf("repoHasPendingChanges() = (%v, %v), want (false, nil)", changed, err)
		}
	})

	t.Run("head changed since baseline is true", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", BaselineHead: "aaaa"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "bbbb\n"}},
		}}
		h := New(fake)
		changed, err := h.repoHasPendingChanges(context.Background(), repo, "## main...origin/main\n")
		if err != nil || !changed {
			t.Fatalf("repoHasPendingChanges() = (%v, %v), want (true, nil)", changed, err)
		}
	})

	t.Run("reused branch without create falls back to false", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", CreateWorkBranch: false}
		h := New(&fakeRunner{t: t})
		changed, err := h.repoHasPendingChanges(context.Background(), repo, "## main...origin/main\n")
		if err != nil || changed {
			t.Fatalf("repoHasPendingChanges() = (%v, %v), want (false, nil)", changed, err)
		}
	})

	t.Run("generated branch defers to commits-ahead check", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", CreateWorkBranch: true, BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: commitsAheadOfBaseCommand(repo.Dir, "main"), res: execx.Result{Stdout: "2\n"}},
		}}
		h := New(fake)
		changed, err := h.repoHasPendingChanges(context.Background(), repo, "## agent00-topic...origin/main\n")
		if err != nil || !changed {
			t.Fatalf("repoHasPendingChanges() = (%v, %v), want (true, nil)", changed, err)
		}
	})
}

func TestH1CovRepoHasPullRequestDelta(t *testing.T) {
	t.Parallel()

	t.Run("reused branch always has delta", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		has, err := h.repoHasPullRequestDelta(context.Background(), repoWorkspace{Dir: "/repo", CreateWorkBranch: false})
		if err != nil || !has {
			t.Fatalf("repoHasPullRequestDelta() = (%v, %v), want (true, nil)", has, err)
		}
	})

	t.Run("generated branch with zero commits ahead has no delta", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", CreateWorkBranch: true, BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: commitsAheadOfBaseCommand(repo.Dir, "main"), res: execx.Result{Stdout: "0\n"}},
		}}
		h := New(fake)
		has, err := h.repoHasPullRequestDelta(context.Background(), repo)
		if err != nil || has {
			t.Fatalf("repoHasPullRequestDelta() = (%v, %v), want (false, nil)", has, err)
		}
	})

	t.Run("count failure propagates", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", CreateWorkBranch: true, BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: commitsAheadOfBaseCommand(repo.Dir, "main"), err: errors.New("boom")},
		}}
		h := New(fake)
		_, err := h.repoHasPullRequestDelta(context.Background(), repo)
		if err == nil {
			t.Fatal("repoHasPullRequestDelta() err = nil, want wrapped count error")
		}
	})
}

func TestH1CovRepoHeadChangedSinceBaseline(t *testing.T) {
	t.Parallel()

	t.Run("empty baseline is never changed", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		changed, err := h.repoHeadChangedSinceBaseline(context.Background(), repoWorkspace{Dir: "/repo"})
		if err != nil || changed {
			t.Fatalf("repoHeadChangedSinceBaseline() = (%v, %v), want (false, nil)", changed, err)
		}
	})

	t.Run("current head command failure propagates", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", BaselineHead: "aaaa"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stderr: "fatal: bad revision"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		_, err := h.repoHeadChangedSinceBaseline(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "read current HEAD for repo") {
			t.Fatalf("repoHeadChangedSinceBaseline() err = %v, want wrapped HEAD error", err)
		}
	})

	t.Run("unchanged baseline is false", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", BaselineHead: "aaaa"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "aaaa\n"}},
		}}
		h := New(fake)
		changed, err := h.repoHeadChangedSinceBaseline(context.Background(), repo)
		if err != nil || changed {
			t.Fatalf("repoHeadChangedSinceBaseline() = (%v, %v), want (false, nil)", changed, err)
		}
	})
}

func TestH1CovCurrentHead(t *testing.T) {
	t.Parallel()

	t.Run("command failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stderr: "fatal: bad revision"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		_, err := h.currentHead(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "read current HEAD for repo") {
			t.Fatalf("currentHead() err = %v, want wrapped error", err)
		}
	})

	t.Run("empty stdout is an error", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "\n"}},
		}}
		h := New(fake)
		_, err := h.currentHead(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "returned empty output") {
			t.Fatalf("currentHead() err = %v, want empty-output error", err)
		}
	})

	t.Run("success trims output", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "  abc123  \n"}},
		}}
		h := New(fake)
		head, err := h.currentHead(context.Background(), repo)
		if err != nil || head != "abc123" {
			t.Fatalf("currentHead() = (%q, %v), want (abc123, nil)", head, err)
		}
	})
}

func TestH1CovCountCommitsAheadOfBase(t *testing.T) {
	t.Parallel()

	t.Run("empty base branch is an error", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		_, err := h.countCommitsAheadOfBase(context.Background(), repoWorkspace{Dir: "/repo"}, "")
		if err == nil || !strings.Contains(err.Error(), "base branch is required") {
			t.Fatalf("countCommitsAheadOfBase() err = %v, want base-branch-required error", err)
		}
	})

	t.Run("command failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: commitsAheadOfBaseCommand(repo.Dir, "main"), res: execx.Result{Stderr: "fatal: bad range"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		_, err := h.countCommitsAheadOfBase(context.Background(), repo, "main")
		if err == nil || !strings.Contains(err.Error(), "count commits ahead of base branch") {
			t.Fatalf("countCommitsAheadOfBase() err = %v, want wrapped count error", err)
		}
	})

	t.Run("empty stdout is zero", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: commitsAheadOfBaseCommand(repo.Dir, "main"), res: execx.Result{Stdout: "\n"}},
		}}
		h := New(fake)
		count, err := h.countCommitsAheadOfBase(context.Background(), repo, "main")
		if err != nil || count != 0 {
			t.Fatalf("countCommitsAheadOfBase() = (%d, %v), want (0, nil)", count, err)
		}
	})

	t.Run("non-numeric output is a parse error", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: commitsAheadOfBaseCommand(repo.Dir, "main"), res: execx.Result{Stdout: "not-a-number\n"}},
		}}
		h := New(fake)
		_, err := h.countCommitsAheadOfBase(context.Background(), repo, "main")
		if err == nil || !strings.Contains(err.Error(), "parse commits-ahead count") {
			t.Fatalf("countCommitsAheadOfBase() err = %v, want parse error", err)
		}
	})

	t.Run("success parses count", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: commitsAheadOfBaseCommand(repo.Dir, "main"), res: execx.Result{Stdout: "3\n"}},
		}}
		h := New(fake)
		count, err := h.countCommitsAheadOfBase(context.Background(), repo, "main")
		if err != nil || count != 3 {
			t.Fatalf("countCommitsAheadOfBase() = (%d, %v), want (3, nil)", count, err)
		}
	})
}

// ---- syncGeneratedWorkBranchWithBase guard clauses not covered elsewhere ----

func TestH1CovSyncGeneratedWorkBranchWithBaseGuards(t *testing.T) {
	t.Parallel()

	t.Run("nil repo", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		exitCode, stage, err := h.syncGeneratedWorkBranchWithBase(context.Background(), nil, agentruntime.Default(), "/codex", codexRunOptions{}, "prompt", "", "", ".", "codex")
		if err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
			t.Fatalf("syncGeneratedWorkBranchWithBase(nil) err = %v, want workspace required error", err)
		}
		if exitCode != ExitConfig || stage != "config" {
			t.Fatalf("syncGeneratedWorkBranchWithBase(nil) = (%d, %q), want (%d, config)", exitCode, stage, ExitConfig)
		}
	})

	t.Run("empty base branch", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", CreateWorkBranch: true}
		h := New(&fakeRunner{t: t})
		exitCode, stage, err := h.syncGeneratedWorkBranchWithBase(context.Background(), repo, agentruntime.Default(), "/codex", codexRunOptions{}, "prompt", "", "", ".", "codex")
		if err == nil || !strings.Contains(err.Error(), "base branch is required") {
			t.Fatalf("syncGeneratedWorkBranchWithBase() err = %v, want base-branch-required error", err)
		}
		if exitCode != ExitGit || stage != "git" {
			t.Fatalf("syncGeneratedWorkBranchWithBase() = (%d, %q), want (%d, git)", exitCode, stage, ExitGit)
		}
	})

	t.Run("fetch failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repoDir := "/repo"
		repo := &repoWorkspace{Dir: repoDir, URL: "git@github.com:acme/repo.git", CreateWorkBranch: true, BaseBranch: "main", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: fetchBranchCommand(repoDir, "main"), res: execx.Result{Stderr: "fatal: unable to access"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		exitCode, stage, err := h.syncGeneratedWorkBranchWithBase(context.Background(), repo, agentruntime.Default(), repoDir, codexRunOptions{}, "prompt", "", "", ".", "codex")
		if err == nil || !strings.Contains(err.Error(), "fetch latest base branch") {
			t.Fatalf("syncGeneratedWorkBranchWithBase() err = %v, want wrapped fetch error", err)
		}
		if exitCode != ExitGit || stage != "git" {
			t.Fatalf("syncGeneratedWorkBranchWithBase() = (%d, %q), want (%d, git)", exitCode, stage, ExitGit)
		}
	})

	t.Run("non-conflict merge failure is wrapped without agent remediation", func(t *testing.T) {
		t.Parallel()
		repoDir := "/repo"
		repo := &repoWorkspace{Dir: repoDir, URL: "git@github.com:acme/repo.git", CreateWorkBranch: true, BaseBranch: "main", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: fetchBranchCommand(repoDir, "main")},
			{cmd: mergeFetchedBranchCommand(repoDir), res: execx.Result{Stderr: "fatal: refusing to merge unrelated histories"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		exitCode, stage, err := h.syncGeneratedWorkBranchWithBase(context.Background(), repo, agentruntime.Default(), repoDir, codexRunOptions{}, "prompt", "", "", ".", "codex")
		if err == nil || !strings.Contains(err.Error(), "merge latest base branch") {
			t.Fatalf("syncGeneratedWorkBranchWithBase() err = %v, want wrapped merge error", err)
		}
		if exitCode != ExitGit || stage != "git" {
			t.Fatalf("syncGeneratedWorkBranchWithBase() = (%d, %q), want (%d, git)", exitCode, stage, ExitGit)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d (should not attempt agent remediation for non-conflict merge errors)", len(fake.exps))
		}
	})
}

// ---- recreatePullRequestIfClosed ----

func TestH1CovRecreatePullRequestIfClosed(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()

	t.Run("nil repo", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		_, err := h.recreatePullRequestIfClosed(context.Background(), nil, cfg)
		if err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
			t.Fatalf("recreatePullRequestIfClosed(nil) err = %v, want workspace required error", err)
		}
	})

	t.Run("reused branch without PR URL is a no-op", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", CreateWorkBranch: false, PRURL: "https://github.com/acme/repo/pull/1"}
		h := New(&fakeRunner{t: t})
		recreated, err := h.recreatePullRequestIfClosed(context.Background(), repo, cfg)
		if err != nil || recreated {
			t.Fatalf("recreatePullRequestIfClosed() = (%v, %v), want (false, nil)", recreated, err)
		}
	})

	t.Run("empty PR URL is a no-op", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", CreateWorkBranch: true}
		h := New(&fakeRunner{t: t})
		recreated, err := h.recreatePullRequestIfClosed(context.Background(), repo, cfg)
		if err != nil || recreated {
			t.Fatalf("recreatePullRequestIfClosed() = (%v, %v), want (false, nil)", recreated, err)
		}
	})

	t.Run("state lookup failure is tolerated", func(t *testing.T) {
		t.Parallel()
		prURL := "https://github.com/acme/repo/pull/2"
		repo := &repoWorkspace{Dir: "/repo", CreateWorkBranch: true, PRURL: prURL}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prStateViewCommand(repo.Dir, prURL), res: execx.Result{Stderr: "fatal: not found"}, err: errors.New("exit status 1")},
		}}
		h := New(fake)
		recreated, err := h.recreatePullRequestIfClosed(context.Background(), repo, cfg)
		if err != nil || recreated {
			t.Fatalf("recreatePullRequestIfClosed() = (%v, %v), want (false, nil)", recreated, err)
		}
	})

	t.Run("open PR is left alone", func(t *testing.T) {
		t.Parallel()
		prURL := "https://github.com/acme/repo/pull/3"
		repo := &repoWorkspace{Dir: "/repo", CreateWorkBranch: true, PRURL: prURL}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prStateViewCommand(repo.Dir, prURL), res: execx.Result{Stdout: `{"url":"` + prURL + `","state":"open"}`}},
		}}
		h := New(fake)
		recreated, err := h.recreatePullRequestIfClosed(context.Background(), repo, cfg)
		if err != nil || recreated {
			t.Fatalf("recreatePullRequestIfClosed() = (%v, %v), want (false, nil)", recreated, err)
		}
		if repo.PRURL != prURL {
			t.Fatalf("PRURL = %q, want unchanged %q", repo.PRURL, prURL)
		}
	})

	t.Run("closed PR is recreated on a renamed branch", func(t *testing.T) {
		t.Parallel()
		repoDir := "/repo"
		oldPRURL := "https://github.com/acme/repo/pull/4"
		newPRURL := "https://github.com/acme/repo/pull/5"
		branch := "agent00-build-api"
		renamedBranch := "agent00-build-api-retry"
		repo := &repoWorkspace{
			URL: cfg.RepoURL, Dir: repoDir, RelDir: "repo", Branch: branch, BaseBranch: cfg.BaseBranch,
			CreateWorkBranch: true, PRURL: oldPRURL, BranchCollisionSuffix: "retry",
		}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prStateViewCommand(repoDir, oldPRURL), res: execx.Result{Stdout: `{"url":"` + oldPRURL + `","state":"closed"}`}},
			{cmd: branchMoveCommand(repoDir, renamedBranch)},
			{cmd: pushToRemoteCommand(repoDir, publishRemoteOrigin, renamedBranch)},
			{cmd: prCreateWithOptionsCommand(repoDir, cfg, cfg.BaseBranch, renamedBranch, ""), res: execx.Result{Stdout: newPRURL + "\n"}},
		}}
		h := New(fake)
		recreated, err := h.recreatePullRequestIfClosed(context.Background(), repo, cfg)
		if err != nil {
			t.Fatalf("recreatePullRequestIfClosed() err = %v, want nil", err)
		}
		if !recreated {
			t.Fatal("recreatePullRequestIfClosed() recreated = false, want true")
		}
		if repo.Branch != renamedBranch {
			t.Fatalf("Branch = %q, want %q", repo.Branch, renamedBranch)
		}
		if repo.PRURL != newPRURL {
			t.Fatalf("PRURL = %q, want %q", repo.PRURL, newPRURL)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("push failure after rename propagates", func(t *testing.T) {
		t.Parallel()
		repoDir := "/repo"
		oldPRURL := "https://github.com/acme/repo/pull/6"
		branch := "agent00-build-api"
		renamedBranch := "agent00-build-api-retry"
		repo := &repoWorkspace{
			URL: cfg.RepoURL, Dir: repoDir, RelDir: "repo", Branch: branch, BaseBranch: cfg.BaseBranch,
			CreateWorkBranch: true, PRURL: oldPRURL, BranchCollisionSuffix: "retry",
		}
		pushErr := errors.New("push failed")
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prStateViewCommand(repoDir, oldPRURL), res: execx.Result{Stdout: `{"url":"` + oldPRURL + `","state":"closed"}`}},
			{cmd: branchMoveCommand(repoDir, renamedBranch)},
			{cmd: pushToRemoteCommand(repoDir, publishRemoteOrigin, renamedBranch), err: pushErr},
		}}
		h := New(fake)
		recreated, err := h.recreatePullRequestIfClosed(context.Background(), repo, cfg)
		if recreated {
			t.Fatal("recreatePullRequestIfClosed() recreated = true, want false")
		}
		if err == nil {
			t.Fatal("recreatePullRequestIfClosed() err = nil, want push failure")
		}
	})
}

// ---- bootstrapUninitializedMainBranch ----

func TestH1CovBootstrapUninitializedMainBranch(t *testing.T) {
	t.Parallel()

	t.Run("success without signing leaves bootstrap commit args untouched", func(t *testing.T) {
		t.Parallel()
		repoDir := t.TempDir()
		repo := repoWorkspace{Dir: repoDir, RelDir: "repo"}
		repoURL := "git@github.com:acme/empty-repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: cloneRepoDefaultBranchCommand(repoURL, repoDir)},
			{cmd: switchMainBranchCommand(repoDir)},
			{cmd: initializeMainBranchCommitCommand(repoDir)},
			{cmd: pushCommand(repoDir, "main")},
		}}
		h := New(fake)
		if err := h.bootstrapUninitializedMainBranch(context.Background(), repo, repoURL); err != nil {
			t.Fatalf("bootstrapUninitializedMainBranch() err = %v, want nil", err)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("clone retry failure propagates", func(t *testing.T) {
		t.Parallel()
		repoDir := t.TempDir()
		repo := repoWorkspace{Dir: repoDir, RelDir: "repo"}
		repoURL := "git@github.com:acme/empty-repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: cloneRepoDefaultBranchCommand(repoURL, repoDir),
				res: execx.Result{Stderr: "fatal: repository not found"},
				err: errors.New("exit status 128"),
			},
		}}
		h := New(fake)
		if err := h.bootstrapUninitializedMainBranch(context.Background(), repo, repoURL); err == nil {
			t.Fatal("bootstrapUninitializedMainBranch() err = nil, want clone failure")
		}
	})

	t.Run("switch branch failure propagates", func(t *testing.T) {
		t.Parallel()
		repoDir := t.TempDir()
		repo := repoWorkspace{Dir: repoDir, RelDir: "repo"}
		repoURL := "git@github.com:acme/empty-repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: cloneRepoDefaultBranchCommand(repoURL, repoDir)},
			{cmd: switchMainBranchCommand(repoDir), err: errors.New("switch failed")},
		}}
		h := New(fake)
		if err := h.bootstrapUninitializedMainBranch(context.Background(), repo, repoURL); err == nil {
			t.Fatal("bootstrapUninitializedMainBranch() err = nil, want switch failure")
		}
	})

	t.Run("push failure propagates", func(t *testing.T) {
		t.Parallel()
		repoDir := t.TempDir()
		repo := repoWorkspace{Dir: repoDir, RelDir: "repo"}
		repoURL := "git@github.com:acme/empty-repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: cloneRepoDefaultBranchCommand(repoURL, repoDir)},
			{cmd: switchMainBranchCommand(repoDir)},
			{cmd: initializeMainBranchCommitCommand(repoDir)},
			{cmd: pushCommand(repoDir, "main"), err: errors.New("push failed")},
		}}
		h := New(fake)
		if err := h.bootstrapUninitializedMainBranch(context.Background(), repo, repoURL); err == nil {
			t.Fatal("bootstrapUninitializedMainBranch() err = nil, want push failure")
		}
	})
}

// ---- cloneRepository: non-default-branch guard clauses not covered elsewhere ----

func TestH1CovCloneRepositoryNilRepo(t *testing.T) {
	t.Parallel()

	h := New(&fakeRunner{t: t})
	if err := h.cloneRepository(context.Background(), nil, "main", nil); err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
		t.Fatalf("cloneRepository(nil) err = %v, want workspace required error", err)
	}
}

func TestH1CovCloneRepositoryRequiresNonDefaultBranchGuards(t *testing.T) {
	t.Parallel()

	t.Run("missing required branch", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", RequiresNonDefaultBranch: true, RequiredBranchTask: "feature-task"}
		h := New(&fakeRunner{t: t})
		err := h.cloneRepository(context.Background(), repo, "release/2026", nil)
		if err == nil || !strings.Contains(err.Error(), "requires a non-default branch before clone") {
			t.Fatalf("cloneRepository() err = %v, want missing-required-branch error", err)
		}
	})

	t.Run("protected required branch", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", RequiresNonDefaultBranch: true, RequiredBranch: "main", RequiredBranchTask: "feature-task"}
		h := New(&fakeRunner{t: t})
		err := h.cloneRepository(context.Background(), repo, "main", nil)
		if err == nil || !strings.Contains(err.Error(), "protected default branch name") {
			t.Fatalf("cloneRepository() err = %v, want protected-branch error", err)
		}
	})

	t.Run("requested branch mismatch", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", RequiresNonDefaultBranch: true, RequiredBranch: "release/2026", RequiredBranchTask: "feature-task"}
		h := New(&fakeRunner{t: t})
		err := h.cloneRepository(context.Background(), repo, "release/2025", nil)
		if err == nil || !strings.Contains(err.Error(), "requires branch") {
			t.Fatalf("cloneRepository() err = %v, want branch-mismatch error", err)
		}
	})
}

func TestH1CovCloneRepositoryFallsBackToDefaultBranchWhenRequestedBranchMissing(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	repo := &repoWorkspace{URL: "git@github.com:acme/repo.git", Dir: repoDir, RelDir: "repo"}
	branch := "agent00-feature"
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{
			cmd: cloneRepoCommand(repo.URL, branch, repoDir),
			res: execx.Result{Stderr: "fatal: Remote branch agent00-feature not found in upstream origin"},
			err: errors.New("exit status 128"),
		},
		{cmd: cloneRepoDefaultBranchCommand(repo.URL, repoDir)},
		{cmd: currentBranchCommand(repoDir), res: execx.Result{Stdout: "main\n"}},
		{cmd: headCommitSHACommand(repoDir)},
	}}
	h := New(fake)
	err := h.cloneRepository(context.Background(), repo, branch, nil)
	if err != nil {
		t.Fatalf("cloneRepository() err = %v, want nil", err)
	}
	if err == nil {
		if repo.BaseBranch != "main" {
			t.Fatalf("cloneRepository() repo.BaseBranch = %q, want main", repo.BaseBranch)
		}
		if repo.CreateWorkBranch != true {
			t.Fatalf("cloneRepository() repo.CreateWorkBranch = %v, want true", repo.CreateWorkBranch)
		}
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}
}
