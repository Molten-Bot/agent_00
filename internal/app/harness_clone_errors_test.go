package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestCloneErrorWithDetails(t *testing.T) {
	t.Parallel()

	if got := cloneErrorWithDetails(nil, execx.Result{}); got != nil {
		t.Fatalf("cloneErrorWithDetails(nil) = %v, want nil", got)
	}

	baseErr := errors.New("clone failed")
	if got := cloneErrorWithDetails(baseErr, execx.Result{}); got != baseErr {
		t.Fatalf("cloneErrorWithDetails(no detail) should return original error")
	}

	got := cloneErrorWithDetails(baseErr, execx.Result{Stderr: " fatal: repository not found "})
	if !errors.Is(got, baseErr) {
		t.Fatalf("cloneErrorWithDetails() should wrap original error: %v", got)
	}
	if !strings.Contains(got.Error(), "fatal: repository not found") {
		t.Fatalf("cloneErrorWithDetails() = %q, want detail text", got.Error())
	}
}

func TestSummarizeCloneErrorDetail(t *testing.T) {
	t.Parallel()

	if got := summarizeCommandErrorDetail(execx.Result{}, maxCloneErrorDetailChars); got != "" {
		t.Fatalf("summarizeCommandErrorDetail(empty) = %q, want empty", got)
	}

	normalized := summarizeCommandErrorDetail(execx.Result{
		Stderr: "fatal:\r\n  repository\r not   found",
		Stdout: "\n warning:   check URL ",
	}, maxCloneErrorDetailChars)
	if got, want := normalized, "fatal: repository not found warning: check URL"; got != want {
		t.Fatalf("summarizeCommandErrorDetail(normalized) = %q, want %q", got, want)
	}

	longInput := strings.Repeat("x", maxCloneErrorDetailChars+40)
	truncated := summarizeCommandErrorDetail(execx.Result{Stderr: longInput}, maxCloneErrorDetailChars)
	if !strings.HasSuffix(truncated, "...(truncated)") {
		t.Fatalf("summarizeCommandErrorDetail(truncated) missing suffix: %q", truncated)
	}
	if got, want := len(truncated), maxCloneErrorDetailChars+len("...(truncated)"); got != want {
		t.Fatalf("len(truncated) = %d, want %d", got, want)
	}
}

func TestIsMissingRemoteBranchCloneError(t *testing.T) {
	t.Parallel()

	if isMissingRemoteBranchCloneError(nil, execx.Result{}) {
		t.Fatal("isMissingRemoteBranchCloneError(nil err) = true, want false")
	}

	errMissing := errors.New("fatal: Could not find remote branch release/2026.04-hotfix to clone")
	if !isMissingRemoteBranchCloneError(errMissing, execx.Result{}) {
		t.Fatal("isMissingRemoteBranchCloneError(could not find remote branch) = false, want true")
	}

	errNotFound := errors.New("clone failed")
	resNotFound := execx.Result{Stderr: "warning: remote branch release/2026.04-hotfix not found in upstream origin"}
	if !isMissingRemoteBranchCloneError(errNotFound, resNotFound) {
		t.Fatal("isMissingRemoteBranchCloneError(remote branch + not found) = false, want true")
	}

	errOther := errors.New("connection timeout")
	if isMissingRemoteBranchCloneError(errOther, execx.Result{Stderr: "unable to access repository"}) {
		t.Fatal("isMissingRemoteBranchCloneError(unrelated error) = true, want false")
	}

	errMissingRef := errors.New("fetch failed")
	resMissingRef := execx.Result{Stderr: "fatal: couldn't find remote ref feature/topic"}
	if !isMissingRemoteBranchCloneError(errMissingRef, resMissingRef) {
		t.Fatal("isMissingRemoteBranchCloneError(missing remote ref) = false, want true")
	}
}

func TestCloneRepositoriesRetainsRequiredBranchCheckoutForValidationWhenRemoteBranchDisappearsAfterClone(t *testing.T) {
	t.Parallel()

	repoURL := "https://github.com/acme/repo.git"
	branch := "feature/topic"
	repoDir := filepath.Join(t.TempDir(), "repo")
	repos := []repoWorkspace{{
		URL:                      repoURL,
		Dir:                      repoDir,
		RelDir:                   "repo",
		RequiresNonDefaultBranch: true,
		RequiredBranch:           branch,
		RequiredBranchTask:       "branch-task",
	}}
	missingRef := execx.Result{Stderr: "fatal: couldn't find remote ref feature/topic"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: cloneRepoCommand(repoURL, branch, repoDir)},
		{cmd: fetchBaseBranchCommand(repoDir, branch), res: missingRef, err: errors.New("exit status 128")},
	}}

	if err := New(fake).cloneRepositories(context.Background(), repos, branch); err != nil {
		t.Fatalf("cloneRepositories() error = %v, want retained checkout", err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}
}

func TestCloneRepositoriesRejectsDeletedResolvePRCommentsBranch(t *testing.T) {
	t.Parallel()

	repoURL := "https://github.com/acme/repo.git"
	branch := "feature/topic"
	repoDir := filepath.Join(t.TempDir(), "repo")
	repos := []repoWorkspace{{
		URL:                repoURL,
		Dir:                repoDir,
		RelDir:             "repo",
		RequiredBranchTask: resolvePRCommentsLibraryTaskName,
	}}
	missingRef := execx.Result{Stderr: "fatal: couldn't find remote ref feature/topic"}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: cloneRepoCommand(repoURL, branch, repoDir)},
		{cmd: fetchBaseBranchCommand(repoDir, branch), res: missingRef, err: errors.New("exit status 128")},
	}}

	if err := New(fake).cloneRepositories(context.Background(), repos, branch); err == nil {
		t.Fatal("cloneRepositories() error = nil, want deleted branch failure")
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}
}

func TestIsRepoNotFoundCloneError(t *testing.T) {
	t.Parallel()

	if isRepoNotFoundCloneError(nil, execx.Result{}) {
		t.Fatal("isRepoNotFoundCloneError(nil err) = true, want false")
	}

	tests := []struct {
		name string
		err  error
		res  execx.Result
		want bool
	}{
		{
			name: "remote repository not found",
			err:  errors.New("clone failed"),
			res:  execx.Result{Stderr: "remote: Repository not found."},
			want: true,
		},
		{
			name: "fatal repository not found",
			err:  errors.New("clone failed"),
			res:  execx.Result{Stderr: "fatal: repository 'git@github.com:acme/repo.git' not found"},
			want: true,
		},
		{
			name: "generic repository not found",
			err:  errors.New("repository not found"),
			res:  execx.Result{},
			want: true,
		},
		{
			name: "not a git repository",
			err:  errors.New("does not appear to be a git repository"),
			res:  execx.Result{},
			want: true,
		},
		{
			name: "repository does not exist",
			err:  errors.New("repository does not exist"),
			res:  execx.Result{},
			want: true,
		},
		{
			name: "unrelated clone failure",
			err:  errors.New("failed to connect to github.com"),
			res:  execx.Result{},
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isRepoNotFoundCloneError(tt.err, tt.res); got != tt.want {
				t.Fatalf("isRepoNotFoundCloneError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRepoOwnerFallbackURL(t *testing.T) {
	t.Parallel()

	repoURL := "git@github.com:moltenbot000/agent_00.git"
	hints := repoOwnerFallbackCandidates([]string{
		"git@github.com:Molten-Bot/user-portal.git",
		"git@github.com:Molten-Bot/agent_00.git",
	})

	got, ok := repoOwnerFallbackURL(repoURL, hints)
	if !ok {
		t.Fatal("repoOwnerFallbackURL() ok = false, want true")
	}
	if got != config.DefaultRepositoryURL {
		t.Fatalf("repoOwnerFallbackURL() = %q, want %q", got, config.DefaultRepositoryURL)
	}
}

func TestRepoOwnerFallbackURLFindsRenamedCanonicalOwner(t *testing.T) {
	t.Parallel()

	got, ok := repoOwnerFallbackURL("git@github.com:moltenbot000/agent_00.git", nil)
	if !ok {
		t.Fatal("repoOwnerFallbackURL() ok = false, want true")
	}
	if got != config.DefaultRepositoryURL {
		t.Fatalf("repoOwnerFallbackURL() = %q, want %q", got, config.DefaultRepositoryURL)
	}
}

func TestRepoOwnerFallbackURLNoCandidate(t *testing.T) {
	t.Parallel()

	repoURL := "git@github.com:acme/private-repo.git"
	if got, ok := repoOwnerFallbackURL(repoURL, repoOwnerFallbackCandidates([]string{repoURL})); ok || got != "" {
		t.Fatalf("repoOwnerFallbackURL() = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestParseGitHubRepoRefSupportsSSHAndHTTPS(t *testing.T) {
	t.Parallel()

	if ref, ok := parseGitHubRepoRef("git@github.com:Molten-Bot/agent_00.git"); !ok || ref.owner != "Molten-Bot" || ref.name != "agent_00" {
		t.Fatalf("parseGitHubRepoRef(scp) = (%+v, %v), want owner/name parsed", ref, ok)
	}
	if ref, ok := parseGitHubRepoRef("ssh://git@github.com/Molten-Bot/agent_00.git"); !ok || ref.owner != "Molten-Bot" || ref.name != "agent_00" {
		t.Fatalf("parseGitHubRepoRef(ssh URL) = (%+v, %v), want owner/name parsed", ref, ok)
	}
	if ref, ok := parseGitHubRepoRef("https://github.com/Molten-Bot/agent_00.git"); !ok || ref.owner != "Molten-Bot" || ref.name != "agent_00" {
		t.Fatalf("parseGitHubRepoRef(https URL) = (%+v, %v), want owner/name parsed", ref, ok)
	}
}

func TestGitHubRepoRefWithHTTPSOwner(t *testing.T) {
	t.Parallel()

	ref, ok := parseGitHubRepoRef("git@github.com:Molten-Bot/agent_00.git")
	if !ok {
		t.Fatal("parseGitHubRepoRef(scp) = false, want true")
	}
	got, ok := ref.withHTTPSOwner("octocat")
	if !ok {
		t.Fatal("withHTTPSOwner() ok = false, want true")
	}
	if got != "https://github.com/octocat/agent_00.git" {
		t.Fatalf("withHTTPSOwner() = %q, want %q", got, "https://github.com/octocat/agent_00.git")
	}
}

func TestIsAgent00RepositorySupportsSSHandHTTPSURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		repoURL string
		want    bool
	}{
		{name: "SSH", repoURL: config.DefaultRepositoryURL, want: true},
		{name: "HTTPS", repoURL: "https://github.com/Molten-Bot/agent_00.git", want: true},
		{name: "wrong owner", repoURL: "git@github.com:octocat/agent_00.git", want: false},
		{name: "other repository", repoURL: "git@github.com:acme/another-repo.git", want: false},
		{name: "invalid", repoURL: "not-a-repository", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isAgent00Repository(tt.repoURL); got != tt.want {
				t.Fatalf("isAgent00Repository(%q) = %v, want %v", tt.repoURL, got, tt.want)
			}
		})
	}
}

func TestIsForkAlreadyExistsErrorRecognizesAlreadyHaveFork(t *testing.T) {
	t.Parallel()

	err := errors.New("exit status 1")
	res := execx.Result{Stderr: "You already have a fork of acme/repo."}
	if !isForkAlreadyExistsError(res, err) {
		t.Fatal("isForkAlreadyExistsError(already have a fork) = false, want true")
	}
}
