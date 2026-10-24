package localruntime

import (
	"strings"
	"testing"
)

func TestParseRunConfigJSON(t *testing.T) {
	t.Parallel()

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"baseBranch": "main",
		"targetSubdir": ".",
		"prompt": "make a change"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if cfg.RepoURL != "git@github.com:acme/repo.git" {
		t.Fatalf("RepoURL = %q", cfg.RepoURL)
	}
	if cfg.BaseBranch != "main" {
		t.Fatalf("BaseBranch = %q", cfg.BaseBranch)
	}
	if cfg.TargetSubdir != "." {
		t.Fatalf("TargetSubdir = %q", cfg.TargetSubdir)
	}
	if cfg.Prompt != "make a change" {
		t.Fatalf("Prompt = %q", cfg.Prompt)
	}
}

func TestParseRunConfigJSONRejectsSnakeCaseAliases(t *testing.T) {
	t.Parallel()

	_, err := ParseRunConfigJSON([]byte(`{
		"repo_url": "git@github.com:acme/repo.git",
		"base_branch": "main",
		"target_subdir": ".",
		"prompt": "make a change",
		"github_handle": "@octocat",
		"images": [
			{"name":"shot.png","media_type":"image/png","data_base64":"aGVsbG8="}
		]
	}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRunConfigJSONRejectsSnakeCaseLibraryTaskName(t *testing.T) {
	t.Parallel()

	_, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"library_task_name": "unit-test-coverage"
	}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRunConfigJSONRejectsSnakeCaseImageFields(t *testing.T) {
	t.Parallel()

	_, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"prompt": "inspect screenshot",
		"images": [
			{"name":"shot.png","media_type":"image/png","dataBase64":"aGVsbG8="}
		]
	}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "images[0].media_type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRunConfigJSONWithImages(t *testing.T) {
	t.Parallel()

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"prompt": "inspect screenshot",
		"images": [
			{
				"name": "shot.png",
				"mediaType": "image/png",
				"dataBase64": "aGVsbG8="
			}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := len(cfg.Images), 1; got != want {
		t.Fatalf("len(Images) = %d, want %d", got, want)
	}
	if got, want := cfg.Images[0].Name, "shot.png"; got != want {
		t.Fatalf("Images[0].Name = %q, want %q", got, want)
	}
}

func TestParseRunConfigJSONWithLegacyDataURIImages(t *testing.T) {
	t.Parallel()

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"prompt": "inspect screenshot",
		"images": [
			"data:image/png;base64,aGVsbG8="
		]
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := len(cfg.Images), 1; got != want {
		t.Fatalf("len(Images) = %d, want %d", got, want)
	}
	if got, want := cfg.Images[0].MediaType, "image/png"; got != want {
		t.Fatalf("Images[0].MediaType = %q, want %q", got, want)
	}
	if got, want := cfg.Images[0].DataBase64, "aGVsbG8="; got != want {
		t.Fatalf("Images[0].DataBase64 = %q, want %q", got, want)
	}
}

func TestParseRunConfigJSONExpandsLibraryTaskPayload(t *testing.T) {
	// This test resolves the on-disk library catalog and must not race tests that
	// temporarily change process working directory.

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"branch": "release",
		"libraryTaskName": "unit-test-coverage"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := cfg.RepoURL, "git@github.com:acme/repo.git"; got != want {
		t.Fatalf("RepoURL = %q, want %q", got, want)
	}
	if got, want := cfg.BaseBranch, "release"; got != want {
		t.Fatalf("BaseBranch = %q, want %q", got, want)
	}
	if got, want := cfg.LibraryTaskDisplayName, "100% Unit Test Coverage"; got != want {
		t.Fatalf("LibraryTaskDisplayName = %q, want %q", got, want)
	}
	if got := cfg.Prompt; !strings.Contains(got, "100% unit-test coverage") {
		t.Fatalf("Prompt = %q", got)
	}
}

func TestParseRunConfigJSONAppendsLibraryTaskPromptContext(t *testing.T) {
	// This test resolves the on-disk library catalog and must not race tests that
	// temporarily change process working directory.

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"libraryTaskName": "root-cause-bug-fix",
		"promptContext": "Clicking save on the settings page throws a 500."
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := cfg.Prompt, "Additional context from requester:\nClicking save on the settings page throws a 500."; !strings.Contains(got, want) {
		t.Fatalf("Prompt = %q, want to contain %q", got, want)
	}

	cfgNoContext, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"libraryTaskName": "root-cause-bug-fix"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if strings.Contains(cfgNoContext.Prompt, "Additional context from requester:") {
		t.Fatalf("Prompt = %q, want no appended context when promptContext is omitted", cfgNoContext.Prompt)
	}
}

func TestParseRunConfigJSONRequiresBranchForMergeMainLibraryTask(t *testing.T) {
	// This test resolves the on-disk library catalog and must not race tests that
	// temporarily change process working directory.

	_, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"libraryTaskName": "fix-merge-main"
	}`))
	if err == nil || !strings.Contains(err.Error(), `library task "fix-merge-main" requires a non-default base branch`) {
		t.Fatalf("ParseRunConfigJSON(missing branch) error = %v", err)
	}

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"branch": "feature/conflicted",
		"libraryTaskName": "fix-merge-main"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON(feature branch) error = %v", err)
	}
	if got, want := cfg.BaseBranch, "feature/conflicted"; got != want {
		t.Fatalf("BaseBranch = %q, want %q", got, want)
	}
}

func TestParseRunConfigJSONCannotOverrideTrustedLibraryTaskGuardWithCaseAliases(t *testing.T) {
	// This test resolves the on-disk library catalog and must not race tests that
	// temporarily change process working directory.

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"branch": "feature/conflicted",
		"libraryTaskName": "fix-merge-main",
		"librarytaskname": "unit-test-coverage",
		"requiresnondefaultbranch": false
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON(conflicting aliases) error = %v", err)
	}
	if got, want := cfg.LibraryTaskName, "fix-merge-main"; got != want {
		t.Fatalf("LibraryTaskName = %q, want trusted expansion %q", got, want)
	}
	if !cfg.RequiresNonDefaultBranch {
		t.Fatal("RequiresNonDefaultBranch = false, want trusted catalog requirement")
	}
	if !strings.Contains(strings.ToLower(cfg.Prompt), "merge") {
		t.Fatalf("Prompt = %q, want fix-merge-main task prompt", cfg.Prompt)
	}
}

func TestParseRunConfigJSONExpandsLowercaseLibraryTaskAlias(t *testing.T) {
	// This test resolves the on-disk library catalog and must not race tests that
	// temporarily change process working directory.

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"branch": "feature/conflicted",
		"librarytaskname": "fix-merge-main"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON(lowercase task alias) error = %v", err)
	}
	if got, want := cfg.LibraryTaskName, "fix-merge-main"; got != want {
		t.Fatalf("LibraryTaskName = %q, want %q", got, want)
	}
	if !cfg.RequiresNonDefaultBranch {
		t.Fatal("RequiresNonDefaultBranch = false, want catalog requirement")
	}
	if strings.TrimSpace(cfg.Prompt) == "" {
		t.Fatal("Prompt is empty, want expanded library task prompt")
	}
}

func TestParseRunConfigJSONExpandsLibraryTaskPayloadWithReposArray(t *testing.T) {
	// This test resolves the on-disk library catalog and must not race tests that
	// temporarily change process working directory.

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repos": [
			"git@github.com:acme/repo-one.git",
			"git@github.com:acme/repo-two.git"
		],
		"branch": "release",
		"libraryTaskName": "unit-test-coverage"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := len(cfg.Repos), 2; got != want {
		t.Fatalf("len(Repos) = %d, want %d", got, want)
	}
	if got, want := cfg.RepoURL, "git@github.com:acme/repo-one.git"; got != want {
		t.Fatalf("RepoURL = %q, want %q", got, want)
	}
	if got, want := cfg.BaseBranch, "release"; got != want {
		t.Fatalf("BaseBranch = %q, want %q", got, want)
	}
	if got := cfg.Prompt; !strings.Contains(got, "100% unit-test coverage") {
		t.Fatalf("Prompt = %q", got)
	}
}

func TestParseRunConfigJSONExpandsLibraryTaskPayloadAndPreservesReviewConfig(t *testing.T) {
	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"libraryTaskName": "code-review",
		"review": {
			"prNumber": 42,
			"headBranch": "feature/improve-tests"
		}
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if cfg.Review == nil {
		t.Fatal("Review = nil, want non-nil")
	}
	if got, want := cfg.Review.PRNumber, 42; got != want {
		t.Fatalf("Review.PRNumber = %d, want %d", got, want)
	}
	if got, want := cfg.Review.HeadBranch, "feature/improve-tests"; got != want {
		t.Fatalf("Review.HeadBranch = %q, want %q", got, want)
	}
	if got := cfg.Prompt; !strings.Contains(got, "pull-request code review") {
		t.Fatalf("Prompt = %q", got)
	}
}

func TestParseRunConfigJSONExpandsLibraryTaskPayloadAndPreservesImages(t *testing.T) {
	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"libraryTaskName": "unit-test-coverage",
		"images": [
			{
				"name": "shot.png",
				"mediaType": "image/png",
				"dataBase64": "aGVsbG8="
			}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := len(cfg.Images), 1; got != want {
		t.Fatalf("len(Images) = %d, want %d", got, want)
	}
	if got, want := cfg.Images[0].Name, "shot.png"; got != want {
		t.Fatalf("Images[0].Name = %q, want %q", got, want)
	}
	if got := cfg.Prompt; !strings.Contains(got, "100% unit-test coverage") {
		t.Fatalf("Prompt = %q", got)
	}
}

func TestParseRunConfigJSONRejectsAmbiguousPromptAndLibraryTask(t *testing.T) {
	t.Parallel()

	_, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"prompt": "x",
		"libraryTaskName": "unit-test-coverage"
	}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot include both prompt and librarytaskname") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRunConfigJSONWithReposArray(t *testing.T) {
	t.Parallel()

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repos": [
			"git@github.com:acme/repo-one.git",
			"git@github.com:acme/repo-two.git"
		],
		"prompt": "make a cross-repo change"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := len(cfg.Repos), 2; got != want {
		t.Fatalf("len(Repos) = %d, want %d", got, want)
	}
	if cfg.RepoURL != "git@github.com:acme/repo-one.git" {
		t.Fatalf("RepoURL = %q", cfg.RepoURL)
	}
}

func TestParseRunConfigJSONIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"baseBranch": "main",
		"targetSubdir": ".",
		"prompt": "make a change",
		"extra_field": {"ignored": true}
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if cfg.RepoURL != "git@github.com:acme/repo.git" {
		t.Fatalf("RepoURL = %q", cfg.RepoURL)
	}
	if cfg.Prompt != "make a change" {
		t.Fatalf("Prompt = %q", cfg.Prompt)
	}
}

func TestParseRunConfigJSONSupportsGitHubHandleReviewer(t *testing.T) {
	t.Parallel()

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"prompt": "make a change",
		"githubHandle": "@octocat"
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := cfg.GitHubHandle, "octocat"; got != want {
		t.Fatalf("GitHubHandle = %q, want %q", got, want)
	}
	if got, want := len(cfg.Reviewers), 1; got != want {
		t.Fatalf("len(Reviewers) = %d, want %d", got, want)
	}
	if got, want := cfg.Reviewers[0], "octocat"; got != want {
		t.Fatalf("Reviewers[0] = %q, want %q", got, want)
	}
}

func TestParseRunConfigJSONTreatsNoneReviewerAsUnset(t *testing.T) {
	t.Parallel()

	cfg, err := ParseRunConfigJSON([]byte(`{
		"repo": "git@github.com:acme/repo.git",
		"prompt": "make a change",
		"reviewers": ["none"]
	}`))
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if cfg.Reviewers != nil {
		t.Fatalf("Reviewers = %#v, want nil", cfg.Reviewers)
	}
}

func TestParseRunConfigJSONRejectsInvalidPayload(t *testing.T) {
	t.Parallel()

	_, err := ParseRunConfigJSON([]byte(`{`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
