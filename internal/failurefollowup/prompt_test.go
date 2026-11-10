package failurefollowup

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithExecutionContractAppendsSharedContract(t *testing.T) {
	t.Parallel()

	got := WithExecutionContract("Base prompt")
	if !strings.HasPrefix(got, "Base prompt\n\n") {
		t.Fatalf("WithExecutionContract() prefix = %q", got)
	}
	if !strings.Contains(got, ExecutionContract) {
		t.Fatalf("WithExecutionContract() missing shared contract: %q", got)
	}
}

func TestWithExecutionContractIncludesFailureResponseInstruction(t *testing.T) {
	t.Parallel()

	got := WithExecutionContract("Base prompt")
	if !strings.Contains(got, FailureResponseInstruction) {
		t.Fatalf("WithExecutionContract() missing failure response instruction: %q", got)
	}
	if !strings.Contains(got, "Use explicit `Failure:` and `Error details:` fields.") {
		t.Fatalf("WithExecutionContract() missing explicit failure field guidance: %q", got)
	}
}

func TestWithExecutionContractIncludesRuntimeToolingInstruction(t *testing.T) {
	t.Parallel()

	got := WithExecutionContract("Base prompt")
	if !strings.Contains(got, RuntimeToolingInstruction) {
		t.Fatalf("WithExecutionContract() missing runtime-tooling instruction: %q", got)
	}
	for _, want := range []string{
		"Playwright",
		"npm",
		"Python",
		"Go",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("WithExecutionContract() missing runtime-tooling detail %q: %q", want, got)
		}
	}
}

func TestWithExecutionContractIncludesValidationToolingInstruction(t *testing.T) {
	t.Parallel()

	got := WithExecutionContract("Base prompt")
	if !strings.Contains(got, ValidationToolingInstruction) {
		t.Fatalf("WithExecutionContract() missing validation-tooling instruction: %q", got)
	}
	if !strings.Contains(got, "do not use `Failure:` solely for that validation gap") {
		t.Fatalf("WithExecutionContract() missing validation-gap failure guidance: %q", got)
	}
	if !strings.Contains(got, "restricted network access blocking dependency downloads") {
		t.Fatalf("WithExecutionContract() missing restricted-network validation guidance: %q", got)
	}
	if !strings.Contains(got, "missing repository validation script or task") {
		t.Fatalf("WithExecutionContract() missing absent-validation-task guidance: %q", got)
	}
}

func TestWithExecutionContractIncludesActivityPrivacyInstruction(t *testing.T) {
	t.Parallel()

	got := WithExecutionContract("Base prompt")
	if !strings.Contains(got, ActivityPrivacyInstruction) {
		t.Fatalf("WithExecutionContract() missing public-activity privacy instruction: %q", got)
	}
	for _, want := range []string{
		"`gh repo view OWNER/REPO --json isPrivate,nameWithOwner`",
		"Share repo and PR links only when GitHub reports `isPrivate:false`",
		"never share private repository links",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("WithExecutionContract() missing privacy detail %q: %q", want, got)
		}
	}
}

func TestWithExecutionContractIncludesUninitializedRepoInstruction(t *testing.T) {
	t.Parallel()

	got := WithExecutionContract("Base prompt")
	if !strings.Contains(got, UninitializedRepoInstruction) {
		t.Fatalf("WithExecutionContract() missing uninitialized-repo instruction: %q", got)
	}
}

func TestWithExecutionContractIncludesRemoteOperationsHandoff(t *testing.T) {
	t.Parallel()

	got := WithExecutionContract("Base prompt")
	if !strings.Contains(got, RemoteOperationsInstruction) {
		t.Fatalf("WithExecutionContract() missing remote-operations guidance: %q", got)
	}
	for _, want := range []string{"Do not commit, push", "harness performs them", "leave the worktree changes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("WithExecutionContract() missing harness lifecycle ownership %q: %q", want, got)
		}
	}
}

func TestRequiredPromptScopesFailureFollowUpToAgent00(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"fix the underlying agent_00 application issues in this repository",
		"The failed run's original task prompt is withheld and is not supplied here",
		"do not reconstruct it, ask for it, or implement the product change it requested unless that change is required to fix agent_00 failure handling",
	} {
		if !strings.Contains(RequiredPrompt, want) {
			t.Fatalf("RequiredPrompt missing %q: %q", want, RequiredPrompt)
		}
	}
}

func TestRequiredPromptDoesNotTreatTheWithheldPromptAsSuppliedContext(t *testing.T) {
	t.Parallel()

	// The failed run's prompt is redacted before it reaches a follow-up, so the
	// instruction must not tell the agent to read it as context.
	for _, unwanted := range []string{
		"Treat the original task prompt as failure context",
		"as failure context only",
	} {
		if strings.Contains(RequiredPrompt, unwanted) {
			t.Fatalf("RequiredPrompt still points at the withheld prompt %q: %q", unwanted, RequiredPrompt)
		}
	}
}

func TestRedactTaskPromptNeverReturnsTheOriginalPrompt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		prompt string
		want   string
	}{
		{"empty", "", ""},
		{"whitespace", "   \n\t ", ""},
		{"customer prompt", "rename the AcmeCorp billing widget owned by Dana Example", RedactedTaskPromptNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactTaskPrompt(tc.prompt); got != tc.want {
				t.Fatalf("RedactTaskPrompt(%q) = %q, want %q", tc.prompt, got, tc.want)
			}
		})
	}
}

func TestRedactedTaskPromptNoticeExplainsWithheldPrompt(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"redacted by the harness",
		"execute against the agent_00 repository",
		"private repository, personnel, or source details",
	} {
		if !strings.Contains(RedactedTaskPromptNotice, want) {
			t.Fatalf("RedactedTaskPromptNotice missing %q: %q", want, RedactedTaskPromptNotice)
		}
	}
}

func TestRedactedTaskPromptNoticeBarsCopyingLogDetailsIntoAgent00Output(t *testing.T) {
	t.Parallel()

	// The notice sends the agent to the failed run's logs, which carry the same
	// customer details the prompt redaction withholds.
	for _, want := range []string{
		"Those logs can contain the same private details",
		"never copy customer repository names, URLs, branch names, file paths, personnel, or source snippets from them",
		"commit messages, pull request titles or bodies, or comments",
	} {
		if !strings.Contains(RedactedTaskPromptNotice, want) {
			t.Fatalf("RedactedTaskPromptNotice missing log-detail guardrail %q: %q", want, RedactedTaskPromptNotice)
		}
	}
}

func TestRedactFailureDetailReplacesCustomerIdentifiers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		detail string
		want   string
	}{
		{"empty", "", ""},
		{"whitespace", "   \n\t ", ""},
		{
			"ssh remote",
			"clone git@github.com:acme/private-repo.git failed",
			"clone " + RedactedURLPlaceholder + " failed",
		},
		{
			"owner-less ssh remote",
			"clone git@git.acme.example:private-repo.git failed",
			"clone " + RedactedURLPlaceholder + " failed",
		},
		{
			"https remote",
			"fetch https://github.com/acme/private-repo.git returned error: 403",
			"fetch " + RedactedURLPlaceholder + " returned error: 403",
		},
		{
			"pull request url",
			"pr create: https://github.com/acme/private-repo/pull/17 already exists",
			"pr create: " + RedactedURLPlaceholder + " already exists",
		},
		{
			"workspace path",
			"agent exited in /home/developer/git/acme/private-repo",
			"agent exited in " + RedactedPathPlaceholder,
		},
		{
			"bare repo reference",
			"remote: Permission to acme/private-repo.git denied to someone.",
			"remote: Permission to " + RedactedURLPlaceholder + " denied to someone.",
		},
		{
			"prompt derived branch",
			"push rejected for agent00-rename-the-billing-widget",
			"push rejected for " + RedactedBranchPlaceholder,
		},
		{
			"diagnostic wording survives",
			"exit status 1: fatal: could not read from remote repository",
			"exit status 1: fatal: could not read from remote repository",
		},
		{
			"relative agent_00 path survives",
			"build failed in cmd/harness/main.go",
			"build failed in cmd/harness/main.go",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactFailureDetail(tc.detail); got != tc.want {
				t.Fatalf("RedactFailureDetail(%q) = %q, want %q", tc.detail, got, tc.want)
			}
		})
	}
}

func TestRedactFailureDetailRedactsEveryIdentifierInACompositeError(t *testing.T) {
	t.Parallel()

	got := RedactFailureDetail(
		"clone git@github.com:acme/private-repo.git into /home/developer/git/acme/private-repo " +
			"failed on branch agent00-fix-the-billing-widget: see https://github.com/acme/private-repo/pull/17",
	)

	for _, unwanted := range []string{"acme", "private-repo", "developer", "billing-widget", "github.com"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("RedactFailureDetail kept %q: %q", unwanted, got)
		}
	}
	want := "clone " + RedactedURLPlaceholder +
		" into " + RedactedPathPlaceholder +
		" failed on branch " + RedactedBranchPlaceholder +
		": see " + RedactedURLPlaceholder
	if got != want {
		t.Fatalf("RedactFailureDetail() = %q, want %q", got, want)
	}
}

func TestComposePromptUsesFallbackPathsAndContract(t *testing.T) {
	t.Parallel()

	got := ComposePrompt(
		RequiredPrompt,
		nil,
		[]string{
			".log/local/<request timestamp>/<request sequence>",
			".log/local/<request timestamp>/<request sequence>/term",
			".log/local/<request timestamp>/<request sequence>/terminal.log",
		},
		"",
		"Observed failure context:\n- exit_code=40",
	)

	for _, want := range []string{
		RequiredPrompt,
		"Relevant failing log path(s):",
		".log/local/<request timestamp>/<request sequence>",
		".log/local/<request timestamp>/<request sequence>/term",
		".log/local/<request timestamp>/<request sequence>/terminal.log",
		"Observed failure context:",
		FailureResponseInstruction,
		RuntimeToolingInstruction,
		ValidationToolingInstruction,
		ActivityPrivacyInstruction,
		UninitializedRepoInstruction,
		RemoteOperationsInstruction,
		ActionableChangeInstruction,
		NoOpInstruction,
		fmt.Sprintf(`{"repos":["git@github.com:Molten-Bot/agent_00.git"],"targetSubdir":".","prompt":"%s"}`, RequiredPrompt),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ComposePrompt() missing %q: %q", want, got)
		}
	}
}

func TestTaskLogPathsBuildsExpectedLegacyAndCurrentFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("/workspace", ".log", "local")
	got := TaskLogPaths(root, "1775613327-000024")
	want := []string{
		filepath.Join(root, "1775613327", "000024"),
		filepath.Join(root, "1775613327", "000024", "term"),
		filepath.Join(root, "1775613327", "000024", "terminal.log"),
		filepath.Join(root, FallbackLogSubdir, "term"),
		filepath.Join(root, FallbackLogSubdir, "terminal.log"),
	}
	if len(got) != len(want) {
		t.Fatalf("len(TaskLogPaths()) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TaskLogPaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFollowUpTargetingDefaultsToRemoteDefaultAndRoot(t *testing.T) {
	t.Parallel()

	baseBranch, targetSubdir := FollowUpTargeting("", "", "")
	if baseBranch != "" {
		t.Fatalf("baseBranch = %q, want empty for remote default", baseBranch)
	}
	if targetSubdir != "." {
		t.Fatalf("targetSubdir = %q, want %q", targetSubdir, ".")
	}
}

func TestFollowUpTargetingForcesRemoteDefaultAndRootForNonDefaultInputs(t *testing.T) {
	t.Parallel()

	baseBranch, targetSubdir := FollowUpTargeting("release/2026.04-hotfix", "internal/localruntime", "release/2026.04-hotfix")
	if baseBranch != "" {
		t.Fatalf("baseBranch = %q, want empty for remote default", baseBranch)
	}
	if targetSubdir != "." {
		t.Fatalf("targetSubdir = %q, want %q", targetSubdir, ".")
	}
}

func TestFollowUpTargetingUsesRemoteDefaultAndRootWhenBaseIsMain(t *testing.T) {
	t.Parallel()

	baseBranch, targetSubdir := FollowUpTargeting("main", ".", "agent00-fix-issue")
	if baseBranch != "" {
		t.Fatalf("baseBranch = %q, want empty for remote default", baseBranch)
	}
	if targetSubdir != "." {
		t.Fatalf("targetSubdir = %q, want %q", targetSubdir, ".")
	}
}

func TestNonRemediableRepoAccessReasonDetectsGitHub403(t *testing.T) {
	t.Parallel()

	err := errors.New("git: remote: Write access to repository not granted.\nfatal: unable to access 'https://github.com/acme/repo.git/': The requested URL returned error: 403")
	if got := NonRemediableRepoAccessReason(err); got != "write access to repository not granted" {
		t.Fatalf("NonRemediableRepoAccessReason() = %q", got)
	}
}

func TestNonRemediableRepoAccessReasonDetectsAgentRepoRightsFailures(t *testing.T) {
	t.Parallel()

	err := errors.New("target repository git@github.com:acme/private.git doesn't have the rights to pull the code or push a PR")
	if got := NonRemediableRepoAccessReason(err); got != "doesn't have the rights to pull the code" {
		t.Fatalf("NonRemediableRepoAccessReason() = %q", got)
	}
}

func TestNonRemediableRepoAccessReasonDetectsGitHubSSHPermissionDenied(t *testing.T) {
	t.Parallel()

	err := errors.New("ERROR: Permission to JuliusBrussee/caveman.git denied to octocat.\nfatal: Could not read from remote repository.")
	if got := NonRemediableRepoAccessReason(err); got != ".git denied to" {
		t.Fatalf("NonRemediableRepoAccessReason() = %q", got)
	}
}

func TestNonRemediableRepoAccessReasonDetectsGitHubSSHPermissionDeniedWithoutDotGit(t *testing.T) {
	t.Parallel()

	err := errors.New("ERROR: Permission to JuliusBrussee/caveman denied to octocat.\nfatal: Could not read from remote repository.")
	if got := NonRemediableRepoAccessReason(err); got != githubSSHPermissionDeniedReason {
		t.Fatalf("NonRemediableRepoAccessReason() = %q", got)
	}
}

func TestNonRemediableRepoAccessReasonDetectsWorkflowScopeRejection(t *testing.T) {
	t.Parallel()

	err := errors.New("remote: refusing to allow an OAuth App to create or update workflow `.github/workflows/docker-release.yml` without `workflow` scope")
	if got := NonRemediableRepoAccessReason(err); got != "refusing to allow an oauth app to create or update workflow" {
		t.Fatalf("NonRemediableRepoAccessReason() = %q", got)
	}
}
