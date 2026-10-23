package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// Tests in this file target previously low/zero-coverage pure logic and
// thin-wrapper functions in harness.go: branch name parsing/sanitizing,
// review outcome JSON parsing, command builders, and a handful of
// Harness methods that are easy to exercise directly with fakeRunner
// without needing a full Run() integration test.

func TestBranchNameFromHeadRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headRef string
		want    string
	}{
		{"empty", "", ""},
		{"owner qualified", "octocat:agent00-feature", "agent00-feature"},
		{"owner qualified with whitespace", "  octocat:  agent00-feature  ", "agent00-feature"},
		{"owner qualified ref prefixed branch", "octocat:refs/heads/agent00-feature", "agent00-feature"},
		{"plain ref prefixed", "refs/heads/agent00-feature", "agent00-feature"},
		{"origin prefixed", "origin/agent00-feature", "agent00-feature"},
		{"plain branch", "agent00-feature", "agent00-feature"},
		{"whitespace only", "   ", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := branchNameFromHeadRef(tc.headRef); got != tc.want {
				t.Fatalf("branchNameFromHeadRef(%q) = %q, want %q", tc.headRef, got, tc.want)
			}
		})
	}
}

func TestIsReviewOnlyRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{"review config present", config.Config{Review: &config.ReviewConfig{PRNumber: 1}}, true},
		{"library task code-review", config.Config{LibraryTaskName: "code-review"}, true},
		{"library task uppercase underscore", config.Config{LibraryTaskName: "Code_Review"}, true},
		{"library task padded", config.Config{LibraryTaskName: "  code-review  "}, true},
		{"library task other", config.Config{LibraryTaskName: "ship-feature"}, false},
		{"empty config", config.Config{}, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isReviewOnlyRun(tc.cfg); got != tc.want {
				t.Fatalf("isReviewOnlyRun(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

func TestSanitizeBranchSuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", ""},
		{"mixed case underscores", "ABC_123", "abc-123"},
		{"collapses repeated separators", "  --multiple---seps__here..  ", "multiple-seps-here"},
		{"only special characters", "!!!", ""},
		{"unicode falls to separator", "héllo", "h-llo"},
		{"already clean", "retry-guid", "retry-guid"},
		{"leading and trailing separators trimmed", "--abc--", "abc"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeBranchSuffix(tc.value); got != tc.want {
				t.Fatalf("sanitizeBranchSuffix(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestRunBranchCollisionSuffix(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 11, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		now  time.Time
		guid string
		want string
	}{
		{"long guid truncated to 8 chars", now, "ABCDEF1234567", "abcdef12"},
		{"short guid kept as-is", now, "abc123", "abc123"},
		{"invalid guid falls back to timestamp", now, "!!!", "20260411-100000"},
		{"empty guid falls back to timestamp", now, "", "20260411-100000"},
		{"invalid guid and zero time falls back to retry", time.Time{}, "!!!", "retry"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runBranchCollisionSuffix(tc.now, tc.guid); got != tc.want {
				t.Fatalf("runBranchCollisionSuffix(%v, %q) = %q, want %q", tc.now, tc.guid, got, tc.want)
			}
		})
	}
}

func TestBranchCollisionSuffixVariant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		suffix  string
		attempt int
		want    string
	}{
		{"empty suffix defaults to retry", "", 1, "retry"},
		{"first attempt keeps suffix", "abc123", 1, "abc123"},
		{"zero attempt keeps suffix", "abc123", 0, "abc123"},
		{"later attempt appends counter", "abc123", 3, "abc123-3"},
		{"sanitizes dirty suffix before appending", "ABC_123", 2, "abc-123-2"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := branchCollisionSuffixVariant(tc.suffix, tc.attempt); got != tc.want {
				t.Fatalf("branchCollisionSuffixVariant(%q, %d) = %q, want %q", tc.suffix, tc.attempt, got, tc.want)
			}
		})
	}
}

func TestCollisionAvoidanceBranchName(t *testing.T) {
	t.Parallel()

	t.Run("empty branch falls back to default task name", func(t *testing.T) {
		t.Parallel()
		if got, want := collisionAvoidanceBranchName("", "retry"), "agent00-task-retry"; got != want {
			t.Fatalf("collisionAvoidanceBranchName(empty) = %q, want %q", got, want)
		}
	})

	t.Run("already carrying suffix is unchanged", func(t *testing.T) {
		t.Parallel()
		if got, want := collisionAvoidanceBranchName("agent00-task-retry", "retry"), "agent00-task-retry"; got != want {
			t.Fatalf("collisionAvoidanceBranchName(already suffixed) = %q, want %q", got, want)
		}
	})

	t.Run("normal branch gets suffix appended", func(t *testing.T) {
		t.Parallel()
		if got, want := collisionAvoidanceBranchName("agent00-build-api", "abc123"), "agent00-build-api-abc123"; got != want {
			t.Fatalf("collisionAvoidanceBranchName() = %q, want %q", got, want)
		}
	})

	t.Run("very long branch is truncated before appending suffix", func(t *testing.T) {
		t.Parallel()
		longBranch := strings.Repeat("a", 130)
		got := collisionAvoidanceBranchName(longBranch, "retry")
		if !strings.HasSuffix(got, "-retry") {
			t.Fatalf("collisionAvoidanceBranchName(long) = %q, want suffix -retry", got)
		}
		if len(got) > 120 {
			t.Fatalf("collisionAvoidanceBranchName(long) length = %d, want <= 120", len(got))
		}
	})
}

func TestReviewFixDiffCommand(t *testing.T) {
	t.Parallel()

	got := reviewFixDiffCommand("/repo", "  abc123  ")
	want := execx.Command{Dir: "/repo", Name: "git", Args: []string{"diff", "--binary", "abc123"}}
	if got.Name != want.Name || got.Dir != want.Dir || strings.Join(got.Args, " ") != strings.Join(want.Args, " ") {
		t.Fatalf("reviewFixDiffCommand() = %+v, want %+v", got, want)
	}
}

func TestMergeAbortCommand(t *testing.T) {
	t.Parallel()

	got := mergeAbortCommand("/repo")
	want := execx.Command{Dir: "/repo", Name: "git", Args: []string{"merge", "--abort"}}
	if got.Name != want.Name || got.Dir != want.Dir || strings.Join(got.Args, " ") != strings.Join(want.Args, " ") {
		t.Fatalf("mergeAbortCommand() = %+v, want %+v", got, want)
	}
}

func TestStripReviewOutcomeJSON(t *testing.T) {
	t.Parallel()

	t.Run("empty text", func(t *testing.T) {
		t.Parallel()
		if got := stripReviewOutcomeJSON(""); got != "" {
			t.Fatalf("stripReviewOutcomeJSON(empty) = %q, want empty", got)
		}
	})

	t.Run("no fenced block returns original text", func(t *testing.T) {
		t.Parallel()
		text := "just plain commentary, no json here"
		if got := stripReviewOutcomeJSON(text); got != text {
			t.Fatalf("stripReviewOutcomeJSON(no fence) = %q, want %q", got, text)
		}
	})

	t.Run("strips valid outcome JSON block", func(t *testing.T) {
		t.Parallel()
		text := "Before text.\n\n```json\n{\"status\":\"clean\",\"mergeReady\":true,\"summary\":\"ok\"}\n```\n\nAfter text."
		got := stripReviewOutcomeJSON(text)
		if strings.Contains(got, "mergeReady") {
			t.Fatalf("stripReviewOutcomeJSON() = %q, want JSON removed", got)
		}
		if !strings.Contains(got, "Before text.") || !strings.Contains(got, "After text.") {
			t.Fatalf("stripReviewOutcomeJSON() = %q, want surrounding text preserved", got)
		}
	})

	t.Run("fenced block with braces but invalid JSON is left untouched", func(t *testing.T) {
		t.Parallel()
		text := "```json\n{not valid json}\n```"
		if got := stripReviewOutcomeJSON(text); got != text {
			t.Fatalf("stripReviewOutcomeJSON(invalid json block) = %q, want unchanged %q", got, text)
		}
	})
}

func TestParseReviewOutcome(t *testing.T) {
	t.Parallel()

	t.Run("empty text", func(t *testing.T) {
		t.Parallel()
		if _, ok := parseReviewOutcome(""); ok {
			t.Fatal("parseReviewOutcome(empty) ok = true, want false")
		}
	})

	t.Run("malformed fenced JSON falls through to not ok", func(t *testing.T) {
		t.Parallel()
		if _, ok := parseReviewOutcome("```json\n{not valid json}\n```"); ok {
			t.Fatal("parseReviewOutcome(malformed) ok = true, want false")
		}
	})

	t.Run("empty JSON object is not a valid outcome", func(t *testing.T) {
		t.Parallel()
		if _, ok := parseReviewOutcome("```json\n{}\n```"); ok {
			t.Fatal("parseReviewOutcome({}) ok = true, want false")
		}
	})

	t.Run("bare trailing JSON object without fence", func(t *testing.T) {
		t.Parallel()
		text := "Review notes: " + `{"status":"clean","mergeReady":true,"findings":[]}`
		outcome, ok := parseReviewOutcome(text)
		if !ok {
			t.Fatal("parseReviewOutcome(bare trailing) ok = false, want true")
		}
		if outcome.Status != "clean" || !outcome.MergeReady {
			t.Fatalf("outcome = %+v, want clean/mergeReady", outcome)
		}
	})

	t.Run("multiple fenced blocks prefers the last valid one", func(t *testing.T) {
		t.Parallel()
		text := "```json\n{\"status\":\"findings\",\"mergeReady\":false,\"summary\":\"first\"}\n```\n" +
			"more analysis\n" +
			"```json\n{\"status\":\"clean\",\"mergeReady\":true,\"summary\":\"second\"}\n```"
		outcome, ok := parseReviewOutcome(text)
		if !ok {
			t.Fatal("parseReviewOutcome(multi block) ok = false, want true")
		}
		if outcome.Summary != "second" {
			t.Fatalf("outcome.Summary = %q, want %q (last valid block)", outcome.Summary, "second")
		}
	})
}

func TestReviewOutcomeAllowsAutoMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome reviewOutcome
		want    bool
	}{
		{"not merge ready", reviewOutcome{MergeReady: false, Status: "clean"}, false},
		{"merge ready clean no findings", reviewOutcome{MergeReady: true, Status: "clean"}, true},
		{"merge ready case-insensitive status", reviewOutcome{MergeReady: true, Status: "CLEAN"}, true},
		{"merge ready merge-ready status", reviewOutcome{MergeReady: true, Status: "merge-ready"}, true},
		{"merge ready merge_ready status", reviewOutcome{MergeReady: true, Status: "merge_ready"}, true},
		{"merge ready unsupported status", reviewOutcome{MergeReady: true, Status: "findings"}, false},
		{
			"merge ready with low severity finding",
			reviewOutcome{MergeReady: true, Status: "clean", Findings: []reviewFinding{{Severity: "low"}}},
			true,
		},
		{
			"merge ready with critical finding blocks merge",
			reviewOutcome{MergeReady: true, Status: "clean", Findings: []reviewFinding{{Severity: "Critical"}}},
			false,
		},
		{
			"merge ready with medium finding blocks merge",
			reviewOutcome{MergeReady: true, Status: "merge-ready", Findings: []reviewFinding{{Severity: "medium"}}},
			false,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := reviewOutcomeAllowsAutoMerge(tc.outcome); got != tc.want {
				t.Fatalf("reviewOutcomeAllowsAutoMerge(%+v) = %v, want %v", tc.outcome, got, tc.want)
			}
		})
	}
}

func TestReviewWritebackMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  string
	}{
		{"", "summary-comment"},
		{"summary", "summary-comment"},
		{"comment", "summary-comment"},
		{"  Summary-Comment  ", "summary-comment"},
		{"off", "off"},
		{"None", "off"},
		{"DISABLED", "off"},
		{"custom-mode", "custom-mode"},
		{"  Custom-Mode  ", "custom-mode"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			if got := reviewWritebackMode(tc.value); got != tc.want {
				t.Fatalf("reviewWritebackMode(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestReviewMergeMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  string
	}{
		{"merge", "merge"},
		{"MERGE", "merge"},
		{"rebase", "rebase"},
		{"Rebase", "rebase"},
		{"", "squash"},
		{"squash", "squash"},
		{"invalid", "squash"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			if got := reviewMergeMethod(tc.value); got != tc.want {
				t.Fatalf("reviewMergeMethod(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestPRReviewSummaryFallbackCommand(t *testing.T) {
	t.Parallel()

	t.Run("no target repo falls back to pr comment", func(t *testing.T) {
		t.Parallel()
		got := prReviewSummaryFallbackCommand("/repo", "42", "", 0, "/tmp/body.md")
		want := execx.Command{Dir: "/repo", Name: "gh", Args: []string{"pr", "comment", "42", "--body-file", "/tmp/body.md"}}
		if strings.Join(got.Args, " ") != strings.Join(want.Args, " ") {
			t.Fatalf("prReviewSummaryFallbackCommand() = %+v, want %+v", got, want)
		}
	})

	t.Run("target repo with explicit PR number uses issues API", func(t *testing.T) {
		t.Parallel()
		got := prReviewSummaryFallbackCommand("/repo", "42", "acme/repo", 42, "/tmp/body.md")
		want := execx.Command{
			Dir:  "/repo",
			Name: "gh",
			Args: []string{"api", "repos/acme/repo/issues/42/comments", "-F", "body=@/tmp/body.md"},
		}
		if strings.Join(got.Args, " ") != strings.Join(want.Args, " ") {
			t.Fatalf("prReviewSummaryFallbackCommand() = %+v, want %+v", got, want)
		}
	})

	t.Run("target repo with numeric selector derives PR number", func(t *testing.T) {
		t.Parallel()
		got := prReviewSummaryFallbackCommand("/repo", "99", "acme/repo", 0, "/tmp/body.md")
		want := execx.Command{
			Dir:  "/repo",
			Name: "gh",
			Args: []string{"api", "repos/acme/repo/issues/99/comments", "-F", "body=@/tmp/body.md"},
		}
		if strings.Join(got.Args, " ") != strings.Join(want.Args, " ") {
			t.Fatalf("prReviewSummaryFallbackCommand() = %+v, want %+v", got, want)
		}
	})

	t.Run("target repo with non-numeric selector falls back to pr comment with repo flag", func(t *testing.T) {
		t.Parallel()
		got := prReviewSummaryFallbackCommand("/repo", "feature-branch", "acme/repo", 0, "/tmp/body.md")
		want := execx.Command{
			Dir:  "/repo",
			Name: "gh",
			Args: []string{"pr", "comment", "feature-branch", "--body-file", "/tmp/body.md", "--repo", "acme/repo"},
		}
		if strings.Join(got.Args, " ") != strings.Join(want.Args, " ") {
			t.Fatalf("prReviewSummaryFallbackCommand() = %+v, want %+v", got, want)
		}
	})
}

func TestGitHubRepoRefWithOwnerAndName(t *testing.T) {
	t.Parallel()

	t.Run("same owner and name is a no-op", func(t *testing.T) {
		t.Parallel()
		ref, ok := parseGitHubRepoRef("git@github.com:Acme/Repo.git")
		if !ok {
			t.Fatal("parseGitHubRepoRef() ok = false, want true")
		}
		if _, changed := ref.withOwnerAndName("acme", "repo"); changed {
			t.Fatal("withOwnerAndName(same owner/name case-insensitive) changed = true, want false")
		}
	})

	t.Run("empty owner or name rejected", func(t *testing.T) {
		t.Parallel()
		ref, ok := parseGitHubRepoRef("git@github.com:acme/repo.git")
		if !ok {
			t.Fatal("parseGitHubRepoRef() ok = false, want true")
		}
		if _, changed := ref.withOwnerAndName("", "newname"); changed {
			t.Fatal("withOwnerAndName(empty owner) changed = true, want false")
		}
		if _, changed := ref.withOwnerAndName("newowner", ""); changed {
			t.Fatal("withOwnerAndName(empty name) changed = true, want false")
		}
	})

	t.Run("owner or name containing slash or whitespace rejected", func(t *testing.T) {
		t.Parallel()
		ref, ok := parseGitHubRepoRef("git@github.com:acme/repo.git")
		if !ok {
			t.Fatal("parseGitHubRepoRef() ok = false, want true")
		}
		if _, changed := ref.withOwnerAndName("new/owner", "repo"); changed {
			t.Fatal("withOwnerAndName(owner with slash) changed = true, want false")
		}
		if _, changed := ref.withOwnerAndName("owner", "new name"); changed {
			t.Fatal("withOwnerAndName(name with space) changed = true, want false")
		}
	})

	t.Run("scp style with git suffix rewrites owner and name", func(t *testing.T) {
		t.Parallel()
		ref, ok := parseGitHubRepoRef("git@github.com:acme/repo.git")
		if !ok {
			t.Fatal("parseGitHubRepoRef() ok = false, want true")
		}
		got, changed := ref.withOwnerAndName("octocat", "fork-repo")
		if !changed {
			t.Fatal("withOwnerAndName() changed = false, want true")
		}
		if want := "git@github.com:octocat/fork-repo.git"; got != want {
			t.Fatalf("withOwnerAndName() = %q, want %q", got, want)
		}
	})

	t.Run("scp style without git suffix omits it in result", func(t *testing.T) {
		t.Parallel()
		ref, ok := parseGitHubRepoRef("git@github.com:acme/repo")
		if !ok {
			t.Fatal("parseGitHubRepoRef() ok = false, want true")
		}
		got, changed := ref.withOwnerAndName("octocat", "fork-repo")
		if !changed {
			t.Fatal("withOwnerAndName() changed = false, want true")
		}
		if want := "git@github.com:octocat/fork-repo"; got != want {
			t.Fatalf("withOwnerAndName() = %q, want %q", got, want)
		}
	})

	t.Run("https style rewrites owner and name", func(t *testing.T) {
		t.Parallel()
		ref, ok := parseGitHubRepoRef("https://github.com/acme/repo.git")
		if !ok {
			t.Fatal("parseGitHubRepoRef() ok = false, want true")
		}
		got, changed := ref.withOwnerAndName("octocat", "fork-repo")
		if !changed {
			t.Fatal("withOwnerAndName() changed = false, want true")
		}
		if want := "https://github.com/octocat/fork-repo.git"; got != want {
			t.Fatalf("withOwnerAndName() = %q, want %q", got, want)
		}
	})

	t.Run("neither scp nor url style rejected", func(t *testing.T) {
		t.Parallel()
		ref := gitHubRepoRef{owner: "acme", name: "repo"}
		if _, changed := ref.withOwnerAndName("octocat", "fork-repo"); changed {
			t.Fatal("withOwnerAndName(no style) changed = true, want false")
		}
	})
}

func TestAgentInvocationLogMetadataWithRuntimeDefaults(t *testing.T) {
	t.Parallel()

	t.Run("blank run ID is left unchanged", func(t *testing.T) {
		t.Parallel()
		m := agentInvocationLogMetadata{}
		got := m.withRuntimeDefaults(agentruntime.Runtime{Harness: "codex"})
		if got != m {
			t.Fatalf("withRuntimeDefaults(blank run ID) = %+v, want unchanged %+v", got, m)
		}
	})

	t.Run("fills in defaults when missing", func(t *testing.T) {
		t.Parallel()
		m := agentInvocationLogMetadata{RunID: "run-1", RepoDir: "repo-dir"}
		got := m.withRuntimeDefaults(agentruntime.Runtime{Harness: "claude"})
		if got.Harness != "claude" {
			t.Fatalf("Harness = %q, want claude", got.Harness)
		}
		if got.Mode != "implementation" {
			t.Fatalf("Mode = %q, want implementation", got.Mode)
		}
		if got.Attempt != 1 {
			t.Fatalf("Attempt = %d, want 1", got.Attempt)
		}
		if got.Repo != "repo-dir" {
			t.Fatalf("Repo = %q, want repo-dir", got.Repo)
		}
	})

	t.Run("preserves already-populated fields", func(t *testing.T) {
		t.Parallel()
		m := agentInvocationLogMetadata{
			RunID:   "run-1",
			Harness: "codex",
			Mode:    "review",
			Attempt: 4,
			Repo:    "custom-repo",
			RepoDir: "repo-dir",
		}
		got := m.withRuntimeDefaults(agentruntime.Runtime{Harness: "claude"})
		if got != m {
			t.Fatalf("withRuntimeDefaults(fully populated) = %+v, want unchanged %+v", got, m)
		}
	})
}

func TestSyncRepoPublishHeadRef(t *testing.T) {
	t.Parallel()

	t.Run("nil repo is a no-op", func(t *testing.T) {
		t.Parallel()
		syncRepoPublishHeadRef(nil)
	})

	t.Run("empty branch is a no-op", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{PRHeadRef: "stale"}
		syncRepoPublishHeadRef(repo)
		if repo.PRHeadRef != "stale" {
			t.Fatalf("PRHeadRef = %q, want unchanged", repo.PRHeadRef)
		}
	})

	t.Run("explicit head owner wins", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Branch: "agent00-topic", PRHeadOwner: "octocat"}
		syncRepoPublishHeadRef(repo)
		if want := "octocat:agent00-topic"; repo.PRHeadRef != want {
			t.Fatalf("PRHeadRef = %q, want %q", repo.PRHeadRef, want)
		}
	})

	t.Run("fork fallback strategy reuses owner from previous head ref", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{
			Branch:          "agent00-renamed",
			PublishStrategy: publishStrategyForkFallback,
			PRHeadRef:       "octocat:agent00-old",
		}
		syncRepoPublishHeadRef(repo)
		if want := "octocat:agent00-renamed"; repo.PRHeadRef != want {
			t.Fatalf("PRHeadRef = %q, want %q", repo.PRHeadRef, want)
		}
		if repo.PRHeadOwner != "octocat" {
			t.Fatalf("PRHeadOwner = %q, want octocat", repo.PRHeadOwner)
		}
	})

	t.Run("direct publish strategy uses bare branch", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Branch: "agent00-topic", PublishStrategy: publishStrategyDirect}
		syncRepoPublishHeadRef(repo)
		if repo.PRHeadRef != "agent00-topic" {
			t.Fatalf("PRHeadRef = %q, want agent00-topic", repo.PRHeadRef)
		}
	})

	t.Run("fork fallback without parsable owner falls back to bare branch", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{
			Branch:          "agent00-renamed",
			PublishStrategy: publishStrategyForkFallback,
			PRHeadRef:       "no-owner-marker",
		}
		syncRepoPublishHeadRef(repo)
		if repo.PRHeadRef != "agent00-renamed" {
			t.Fatalf("PRHeadRef = %q, want agent00-renamed", repo.PRHeadRef)
		}
	})
}

func TestRemoteBranchSyncConflictErrorMethods(t *testing.T) {
	t.Parallel()

	var nilErr *remoteBranchSyncConflictError
	if got := nilErr.Error(); got != "" {
		t.Fatalf("nil.Error() = %q, want empty", got)
	}
	if got := nilErr.Unwrap(); got != nil {
		t.Fatalf("nil.Unwrap() = %v, want nil", got)
	}

	blank := &remoteBranchSyncConflictError{}
	if got := blank.Error(); got != "" {
		t.Fatalf("blank.Error() = %q, want empty", got)
	}
	if got := blank.Unwrap(); got != nil {
		t.Fatalf("blank.Unwrap() = %v, want nil", got)
	}

	inner := errors.New("merge conflict while syncing")
	populated := &remoteBranchSyncConflictError{err: inner, remote: "origin", branch: "agent00-topic"}
	if got := populated.Error(); got != inner.Error() {
		t.Fatalf("populated.Error() = %q, want %q", got, inner.Error())
	}
	if got := populated.Unwrap(); !errors.Is(got, inner) {
		t.Fatalf("populated.Unwrap() = %v, want %v", got, inner)
	}
	var target *remoteBranchSyncConflictError
	if !errors.As(error(populated), &target) {
		t.Fatal("errors.As() = false, want true")
	}
}

func TestResolveBaseSyncConflictWithAgentStillUnmergedIsFatal(t *testing.T) {
	t.Parallel()

	repoDir := "/repo"
	repo := &repoWorkspace{
		URL:              "git@github.com:acme/repo.git",
		Dir:              repoDir,
		RelDir:           "repo",
		Branch:           "agent00-build-api",
		BaseBranch:       "main",
		CreateWorkBranch: true,
	}
	mergeConflict := execx.Result{Stdout: "CONFLICT (content): Merge conflict in file.go\nAutomatic merge failed; fix conflicts and then commit the result."}
	codexBasePrompt := withBackpressurePrompt("Build API", nil)
	resolvePrompt := baseSyncConflictPrompt(
		codexBasePrompt,
		repo.RelDir,
		repo.URL,
		repo.Branch,
		repo.BaseBranch,
		mergeConflict,
	)

	// Drive the conflict through syncGeneratedWorkBranchWithBase (Run()'s actual
	// call path) rather than invoking resolveBaseSyncConflictWithAgent directly,
	// so the fetch/merge-conflict detection that triggers remediation is covered too.
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: fetchBranchCommand(repoDir, repo.BaseBranch)},
		{cmd: mergeFetchedBranchCommand(repoDir), res: mergeConflict, err: errors.New("exit status 1")},
		{cmd: codexCommand(repoDir, resolvePrompt)},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: "## agent00-build-api\nUU file.go\n"}},
	}}

	h := New(fake)
	exitCode, stage, err := h.syncGeneratedWorkBranchWithBase(
		context.Background(), repo, agentruntime.Default(), repoDir, codexRunOptions{},
		codexBasePrompt, "", "", ".", "codex",
	)
	if err == nil || !strings.Contains(err.Error(), "still has unmerged paths after remediation") {
		t.Fatalf("syncGeneratedWorkBranchWithBase() err = %v, want unmerged-paths fatal error", err)
	}
	if exitCode != ExitGit || stage != "git" {
		t.Fatalf("syncGeneratedWorkBranchWithBase() = (%d, %q), want (%d, \"git\")", exitCode, stage, ExitGit)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}
}

func TestResolveRemoteBranchSyncConflictWithAgentStillUnmergedIsFatal(t *testing.T) {
	t.Parallel()

	repoDir := "/repo"
	repo := &repoWorkspace{
		URL:    "git@github.com:acme/repo.git",
		Dir:    repoDir,
		RelDir: "repo",
		Branch: "agent00-build-api",
	}
	nonFF := execx.Result{Stderr: "! [rejected] non-fast-forward"}
	mergeConflict := execx.Result{Stdout: "CONFLICT (content): Merge conflict in file.go\nAutomatic merge failed; fix conflicts and then commit the result."}
	codexBasePrompt := withBackpressurePrompt("Build API", nil)
	resolvePrompt := remoteBranchSyncConflictPrompt(
		codexBasePrompt,
		repo.RelDir,
		repo.URL,
		repo.Branch,
		publishRemoteOrigin,
		mergeConflict,
	)

	// Drive the conflict through pushWithSyncAndConflictResolution (Run()'s actual
	// call path) rather than invoking resolveRemoteBranchSyncConflictWithAgent
	// directly, so the push/fetch/merge-conflict detection that triggers
	// remediation is covered too.
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: pushToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: nonFF, err: errors.New("exit status 1")},
		{cmd: fetchBranchFromRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch)},
		{cmd: mergeFetchedBranchCommand(repo.Dir), res: mergeConflict, err: errors.New("exit status 1")},
		{cmd: codexCommand(repoDir, resolvePrompt)},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: "## agent00-build-api\nUU file.go\n"}},
	}}

	h := New(fake)
	exitCode, stage, err := h.pushWithSyncAndConflictResolution(
		context.Background(), repo, 0, agentruntime.Default(), repoDir, codexRunOptions{},
		codexBasePrompt, "", "", ".", "codex",
	)
	if err == nil || !strings.Contains(err.Error(), "still has unmerged paths after remediation") {
		t.Fatalf("pushWithSyncAndConflictResolution() err = %v, want unmerged-paths fatal error", err)
	}
	if exitCode != ExitGit || stage != "git" {
		t.Fatalf("pushWithSyncAndConflictResolution() = (%d, %q), want (%d, \"git\")", exitCode, stage, ExitGit)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}
}

func TestReviewSummaryWritebackErrorMethods(t *testing.T) {
	t.Parallel()

	reviewErr := errors.New("review command boom")
	fallbackErr := errors.New("fallback comment boom")
	e := reviewSummaryWritebackError{reviewErr: reviewErr, fallbackErr: fallbackErr}

	want := fmt.Sprintf("review command failed: %v; fallback pull request comment failed: %v", reviewErr, fallbackErr)
	if got := e.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := e.Unwrap(); !errors.Is(got, fallbackErr) {
		t.Fatalf("Unwrap() = %v, want %v", got, fallbackErr)
	}
}

func TestVerifyRemoteWriteAccess(t *testing.T) {
	t.Parallel()

	t.Run("write access granted", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: pushDryRunToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch)},
		}}
		h := New(fake)
		if err := h.verifyRemoteWriteAccess(context.Background(), repo); err != nil {
			t.Fatalf("verifyRemoteWriteAccess() err = %v, want nil", err)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("non-fast-forward is treated as access granted", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: pushDryRunToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch),
				res: execx.Result{Stderr: "! [rejected] non-fast-forward"},
				err: errors.New("exit status 1"),
			},
		}}
		h := New(fake)
		if err := h.verifyRemoteWriteAccess(context.Background(), repo); err != nil {
			t.Fatalf("verifyRemoteWriteAccess() err = %v, want nil", err)
		}
	})

	t.Run("permission denied is a hard error", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: pushDryRunToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch),
				res: execx.Result{Stderr: "remote: Write access to repository not granted."},
				err: errors.New("exit status 128"),
			},
		}}
		h := New(fake)
		err := h.verifyRemoteWriteAccess(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "verify remote write access for repo") {
			t.Fatalf("verifyRemoteWriteAccess() err = %v, want wrapped verify error", err)
		}
	})
}

func TestCommentPRScreenshots(t *testing.T) {
	t.Parallel()

	t.Run("no files is a no-op", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRunner{t: t}
		h := New(fake)
		repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/1", Branch: "agent00-topic"}
		if err := h.commentPRScreenshots(context.Background(), repo, nil); err != nil {
			t.Fatalf("commentPRScreenshots(no files) err = %v, want nil", err)
		}
	})

	t.Run("invalid PR URL surfaces parse error without invoking runner", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRunner{t: t}
		h := New(fake)
		repo := repoWorkspace{Dir: "/repo", PRURL: "not-a-valid-url", Branch: "agent00-topic"}
		err := h.commentPRScreenshots(context.Background(), repo, []string{"shot.png"})
		if err == nil || !strings.Contains(err.Error(), "parse GitHub pull request URL") {
			t.Fatalf("commentPRScreenshots(invalid URL) err = %v, want parse error", err)
		}
	})

	t.Run("missing branch surfaces error without invoking runner", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRunner{t: t}
		h := New(fake)
		repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/1", Branch: ""}
		err := h.commentPRScreenshots(context.Background(), repo, []string{"shot.png"})
		if err == nil || !strings.Contains(err.Error(), "branch is required") {
			t.Fatalf("commentPRScreenshots(no branch) err = %v, want branch required error", err)
		}
	})

	t.Run("posts a comment with the rendered image body", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/1", Branch: "agent00-topic"}
		files := []string{"shot.png"}
		body, err := prCommentScreenshotsBody(repo.PRURL, repo.PRHeadOwner, repo.Branch, files)
		if err != nil {
			t.Fatalf("prCommentScreenshotsBody() err = %v", err)
		}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prCommentCommand(repo.Dir, repo.PRURL, body)},
		}}
		h := New(fake)
		if err := h.commentPRScreenshots(context.Background(), repo, files); err != nil {
			t.Fatalf("commentPRScreenshots() err = %v, want nil", err)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})
}

func TestLatestChecksAreAllPassing(t *testing.T) {
	t.Parallel()

	t.Run("all checks passing via JSON", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/42"}
		snapshot := `[{"name":"build","bucket":"pass","completedAt":"2026-04-02T15:05:00Z"},` +
			`{"name":"deploy","bucket":"pass","completedAt":"2026-04-02T15:05:01Z"}]`
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prChecksJSONCommand(repo.Dir, repo.PRURL, true), res: execx.Result{Stdout: snapshot}},
		}}
		h := New(fake)
		allPassing, _, err := h.latestChecksAreAllPassing(context.Background(), repo, true)
		if err != nil {
			t.Fatalf("latestChecksAreAllPassing() err = %v", err)
		}
		if !allPassing {
			t.Fatal("allPassing = false, want true")
		}
	})

	t.Run("a failing check means not all passing", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/42"}
		snapshot := `[{"name":"build","bucket":"fail","completedAt":"2026-04-02T15:05:00Z"},` +
			`{"name":"deploy","bucket":"pass","completedAt":"2026-04-02T15:05:01Z"}]`
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prChecksJSONCommand(repo.Dir, repo.PRURL, false), res: execx.Result{Stdout: snapshot}},
		}}
		h := New(fake)
		allPassing, _, err := h.latestChecksAreAllPassing(context.Background(), repo, false)
		if err != nil {
			t.Fatalf("latestChecksAreAllPassing() err = %v", err)
		}
		if allPassing {
			t.Fatal("allPassing = true, want false")
		}
	})

	t.Run("falls back to text snapshot when JSON flag unsupported", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/42"}
		checkOutput := "Build and test\tpass\t22s\thttps://github.com/acme/repo/actions/runs/2/job/22"
		unsupportedJSON := "unknown flag: --json\nUsage:  gh pr checks [<number> | <url> | <branch>] [flags]"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{
				cmd: prChecksJSONCommand(repo.Dir, repo.PRURL, true),
				res: execx.Result{Stderr: unsupportedJSON},
				err: errors.New("exit status 1"),
			},
			{cmd: prChecksTextCommand(repo.Dir, repo.PRURL, true), res: execx.Result{Stdout: checkOutput}},
		}}
		h := New(fake)
		allPassing, _, err := h.latestChecksAreAllPassing(context.Background(), repo, true)
		if err != nil {
			t.Fatalf("latestChecksAreAllPassing() err = %v", err)
		}
		if !allPassing {
			t.Fatal("allPassing = false, want true")
		}
	})

	t.Run("unrecoverable command error propagates", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/42"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: prChecksJSONCommand(repo.Dir, repo.PRURL, true), err: errors.New("network unreachable")},
		}}
		h := New(fake)
		_, _, err := h.latestChecksAreAllPassing(context.Background(), repo, true)
		if err == nil || !strings.Contains(err.Error(), "network unreachable") {
			t.Fatalf("latestChecksAreAllPassing() err = %v, want network error", err)
		}
	})
}

func TestResolveClonedDefaultBranch(t *testing.T) {
	t.Parallel()

	t.Run("resolves branch directly", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RelDir: "repo"}
		repoURL := "git@github.com:acme/repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stdout: "main\n"}},
			{cmd: headCommitSHACommand(repo.Dir)},
		}}
		h := New(fake)
		got, err := h.resolveClonedDefaultBranch(context.Background(), repo, repoURL)
		if err != nil {
			t.Fatalf("resolveClonedDefaultBranch() err = %v", err)
		}
		if got != "main" {
			t.Fatalf("resolveClonedDefaultBranch() = %q, want main", got)
		}
	})

	t.Run("current branch command failure is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RelDir: "repo"}
		repoURL := "git@github.com:acme/repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stderr: "fatal: not a git repository"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		_, err := h.resolveClonedDefaultBranch(context.Background(), repo, repoURL)
		if err == nil || !strings.Contains(err.Error(), "resolve cloned default branch for repo") {
			t.Fatalf("resolveClonedDefaultBranch() err = %v, want wrapped branch resolution error", err)
		}
	})

	t.Run("empty branch with existing remote refs is an error", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RelDir: "repo"}
		repoURL := "git@github.com:acme/repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stdout: "\n"}},
			{cmd: headCommitSHACommand(repo.Dir)},
			{cmd: remoteRefsCommand(repoURL), res: execx.Result{Stdout: "abc123\trefs/heads/main\n"}},
		}}
		h := New(fake)
		_, err := h.resolveClonedDefaultBranch(context.Background(), repo, repoURL)
		if err == nil || !strings.Contains(err.Error(), "returned empty branch") {
			t.Fatalf("resolveClonedDefaultBranch() err = %v, want empty branch error", err)
		}
	})

	t.Run("head commit failure with existing refs is wrapped", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RelDir: "repo"}
		repoURL := "git@github.com:acme/repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stdout: "main\n"}},
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stderr: "fatal: bad revision 'HEAD'"}, err: errors.New("exit status 128")},
			{cmd: remoteRefsCommand(repoURL), res: execx.Result{Stdout: "abc123\trefs/heads/main\n"}},
		}}
		h := New(fake)
		_, err := h.resolveClonedDefaultBranch(context.Background(), repo, repoURL)
		if err == nil || !strings.Contains(err.Error(), "verify cloned default branch HEAD for repo") {
			t.Fatalf("resolveClonedDefaultBranch() err = %v, want wrapped HEAD verification error", err)
		}
	})

	t.Run("remote refs lookup failure propagates", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", RelDir: "repo"}
		repoURL := "git@github.com:acme/repo.git"
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: currentBranchCommand(repo.Dir), res: execx.Result{Stdout: "\n"}},
			{cmd: headCommitSHACommand(repo.Dir)},
			{cmd: remoteRefsCommand(repoURL), res: execx.Result{Stderr: "unable to access"}, err: errors.New("exit status 128")},
		}}
		h := New(fake)
		_, err := h.resolveClonedDefaultBranch(context.Background(), repo, repoURL)
		if err == nil || !strings.Contains(err.Error(), "inspect remote refs for repo") {
			t.Fatalf("resolveClonedDefaultBranch() err = %v, want wrapped remote refs error", err)
		}
	})
}

func TestPushWithSync(t *testing.T) {
	t.Parallel()

	t.Run("nil repo requires workspace", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRunner{t: t}
		h := New(fake)
		err := h.pushWithSync(context.Background(), nil, 0, true)
		if err == nil || !strings.Contains(err.Error(), "repo workspace is required") {
			t.Fatalf("pushWithSync(nil repo) err = %v, want workspace required error", err)
		}
	})

	t.Run("push succeeds on first attempt", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: pushToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch)},
		}}
		h := New(fake)
		if err := h.pushWithSync(context.Background(), repo, 0, true); err != nil {
			t.Fatalf("pushWithSync() err = %v, want nil", err)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("non-fast-forward on generated branch renames and retries", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{
			Dir:                   "/repo",
			Branch:                "agent00-topic",
			CreateWorkBranch:      true,
			BranchCollisionSuffix: "retry",
		}
		nonFF := execx.Result{Stderr: "! [rejected] non-fast-forward"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: pushToRemoteCommand(repo.Dir, publishRemoteOrigin, "agent00-topic"), res: nonFF, err: errors.New("exit status 1")},
			{cmd: branchMoveCommand(repo.Dir, "agent00-topic-retry")},
			{cmd: pushToRemoteCommand(repo.Dir, publishRemoteOrigin, "agent00-topic-retry")},
		}}
		h := New(fake)
		if err := h.pushWithSync(context.Background(), repo, 0, true); err != nil {
			t.Fatalf("pushWithSync() err = %v, want nil", err)
		}
		if repo.Branch != "agent00-topic-retry" {
			t.Fatalf("repo.Branch = %q, want agent00-topic-retry", repo.Branch)
		}
		if repo.BranchCollisionAttempts != 1 {
			t.Fatalf("repo.BranchCollisionAttempts = %d, want 1", repo.BranchCollisionAttempts)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("merge conflict with leaveMergeConflicts returns conflict error without aborting", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		nonFF := execx.Result{Stderr: "! [rejected] non-fast-forward"}
		mergeConflict := execx.Result{Stdout: "Automatic merge failed; fix conflicts and then commit the result."}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: pushToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: nonFF, err: errors.New("exit status 1")},
			{cmd: fetchBranchFromRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch)},
			{cmd: mergeFetchedBranchCommand(repo.Dir), res: mergeConflict, err: errors.New("exit status 1")},
		}}
		h := New(fake)
		err := h.pushWithSync(context.Background(), repo, 0, true)
		var conflictErr *remoteBranchSyncConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("pushWithSync() err = %v, want *remoteBranchSyncConflictError", err)
		}
		if conflictErr.remote != publishRemoteOrigin || conflictErr.branch != repo.Branch {
			t.Fatalf("conflictErr = %+v, want remote=%q branch=%q", conflictErr, publishRemoteOrigin, repo.Branch)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("merge conflict without leaveMergeConflicts aborts and still returns conflict error", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		nonFF := execx.Result{Stderr: "! [rejected] non-fast-forward"}
		mergeConflict := execx.Result{Stdout: "Automatic merge failed; fix conflicts and then commit the result."}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: pushToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: nonFF, err: errors.New("exit status 1")},
			{cmd: fetchBranchFromRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch)},
			{cmd: mergeFetchedBranchCommand(repo.Dir), res: mergeConflict, err: errors.New("exit status 1")},
			{cmd: mergeAbortCommand(repo.Dir)},
		}}
		h := New(fake)
		err := h.pushWithSync(context.Background(), repo, 0, false)
		var conflictErr *remoteBranchSyncConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("pushWithSync() err = %v, want *remoteBranchSyncConflictError", err)
		}
		if len(fake.exps) != 0 {
			t.Fatalf("unconsumed expectations: %d", len(fake.exps))
		}
	})

	t.Run("merge conflict abort failure wraps both errors", func(t *testing.T) {
		t.Parallel()
		repo := &repoWorkspace{Dir: "/repo", Branch: "agent00-topic"}
		nonFF := execx.Result{Stderr: "! [rejected] non-fast-forward"}
		mergeConflict := execx.Result{Stdout: "Automatic merge failed; fix conflicts and then commit the result."}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: pushToRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch), res: nonFF, err: errors.New("exit status 1")},
			{cmd: fetchBranchFromRemoteCommand(repo.Dir, publishRemoteOrigin, repo.Branch)},
			{cmd: mergeFetchedBranchCommand(repo.Dir), res: mergeConflict, err: errors.New("exit status 1")},
			{cmd: mergeAbortCommand(repo.Dir), err: errors.New("could not abort")},
		}}
		h := New(fake)
		err := h.pushWithSync(context.Background(), repo, 0, false)
		var conflictErr *remoteBranchSyncConflictError
		if errors.As(err, &conflictErr) {
			t.Fatalf("pushWithSync() err = %v, want wrapped abort failure, not conflict error", err)
		}
		if err == nil || !strings.Contains(err.Error(), "abort merge after push retry conflict") {
			t.Fatalf("pushWithSync() err = %v, want abort failure message", err)
		}
	})
}
