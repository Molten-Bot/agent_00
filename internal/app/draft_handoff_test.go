package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/tasktiming"
	"github.com/Molten-Bot/agent_00/internal/workflow"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestVerifyDraftHandoffWithSuppliedEvidence(t *testing.T) {
	t.Parallel()
	const prURL = "https://github.com/acme/repo/pull/42"
	repo := repoWorkspace{
		Dir: t.TempDir(), PRVerification: draftHandoffStandard,
	}
	const patchText = "diff --git a/work.txt b/work.txt\n--- a/work.txt\n+++ b/work.txt\n@@ -1 +1 @@\n-base\n+implemented\n"
	commits := draftPatchCommits{Base: "reviewed-base", Head: "reviewed-head"}
	patch := &draftReviewPatch{commits: commits, path: filepath.Join(repo.Dir, "reviewed.patch"), size: len(patchText), digest: sha256.Sum256([]byte(patchText))}
	if err := os.WriteFile(patch.path, []byte(patchText), 0o600); err != nil {
		t.Fatal(err)
	}
	repo.ReviewedPatch = patch
	evidence := draftVerificationEvidence{
		commits:        commits,
		requiredChecks: []string{"CI"},
		review: githubutil.SubmittedReview{
			ID: 1, Head: "reviewed-head", URL: prURL + "#pullrequestreview-1", State: "COMMENTED",
			Body: githubutil.CleanReviewVerdict + "\nNo material issues found.",
		},
	}
	if err := json.Unmarshal([]byte(`{
		"number":42,"url":"https://github.com/acme/repo/pull/42","state":"OPEN","isDraft":true,
		"baseRefName":"main","headRefName":"agent00-issue-17","headRefOid":"reviewed-head",
		"statusCheckRollup":[{"name":"CI","conclusion":"SUCCESS"}],
		"changedFiles":1,"files":[{"path":"work.txt","additions":1,"deletions":1}]
	}`), &evidence.metadata); err != nil {
		t.Fatal(err)
	}
	h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if cmd.Name != "codex" {
			return execx.Result{}, fmt.Errorf("unexpected offline verifier command %q", cmd.Name)
		}
		for _, expected := range []string{draftHandoffStandard, "reviewed-head", "No material issues found.", `"conclusion":"SUCCESS"`, `"path":"work.txt"`, "+implemented"} {
			if !strings.Contains(cmd.Stdin, expected) {
				t.Errorf("verification prompt missing %q", expected)
			}
		}
		if !strings.Contains(strings.Join(cmd.Args, " "), "network={enabled=false}") || !slices.Contains(cmd.Args, `web_search="disabled"`) {
			t.Error("verifier was given network access")
		}
		return execx.Result{Stdout: `{"status":"passed","evidence":"The supplied clean review and CI result match the draft head."}`}, nil
	}))
	opts := codexRunOptions{Env: []string{"CODEX_HOME=" + filepath.Join(t.TempDir(), "codex")}}
	if err := h.verifyDraftHandoff(t.Context(), repo, agentruntime.Default(), opts, nil); !errors.Is(err, ErrReviewPending) {
		t.Fatalf("missing evidence must keep review pending: %v", err)
	}
	if err := h.verifyDraftHandoff(t.Context(), repo, agentruntime.Default(), opts, &evidence); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, data, want string
		commits          draftPatchCommits
		sentinel         error
	}{
		{name: "missing", commits: commits, want: "missing reviewed patch evidence file", sentinel: ErrReviewPending},
		{name: "truncated", data: patchText[:30], commits: commits, want: "reviewed patch evidence is truncated", sentinel: ErrReviewPending},
		{name: "replaced", data: strings.Replace(patchText, "implemented", "substituted", 1), commits: commits, want: "does not match the captured patch digest", sentinel: ErrReviewPending},
		{name: "stale base", data: patchText, commits: draftPatchCommits{Base: "new-base", Head: commits.Head}, want: "has a stale draft base", sentinel: ErrReviewPending},
		{name: "different head", data: patchText, commits: draftPatchCommits{Base: commits.Base, Head: "new-head"}, want: "draft head changed", sentinel: ErrReviewHeadChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.data == "" {
				if err := os.Remove(patch.path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(patch.path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			evidence.commits = tc.commits
			err := h.verifyDraftHandoff(t.Context(), repo, agentruntime.Default(), opts, &evidence)
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verification error = %v, want %s (%v)", err, tc.want, tc.sentinel)
			}
		})
	}
}

func TestPrepareDraftReviewPatchBoundsArtifact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := repoWorkspace{Dir: filepath.Join(root, "repo")}
	h := New(issueBranchRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if cmd.Args[0] == "rev-parse" {
			return execx.Result{Stdout: "base"}, nil
		}
		if _, err := cmd.StdoutWriter.Write(make([]byte, maxDraftPatchBytes)); err != nil {
			return execx.Result{}, err
		}
		_, err := cmd.StdoutWriter.Write([]byte("oversized"))
		return execx.Result{}, err
	}))
	patch, _, err := h.prepareDraftReviewPatch(t.Context(), repo, "origin/main", "head")
	if patch != nil || !errors.Is(err, errDraftPatchTooLarge) {
		t.Fatalf("oversized patch should be rejected with a retryable diagnostic: %v, %v", patch, err)
	}
	if entries, err := os.ReadDir(root); len(entries) != 0 || err != nil {
		t.Fatalf("oversized patch should be removed: %v, %v", entries, err)
	}
}

func TestClaimedIssueDraftHandoff(t *testing.T) {
	credentialKeys := []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_CONFIG_DIR", "SSH_AUTH_SOCK", reviewCheckTokenEnv}
	for _, key := range credentialKeys {
		t.Setenv(key, "test-token")
	}
	const (
		branch       = "agent00-issue-17"
		repoURL      = "https://github.com/acme/repo.git"
		prURL        = "https://github.com/acme/repo/pull/42"
		requirement  = "Verify the complete live changed-file inventory, the change to work.txt, the submitted clean review, and the CI result for the draft head."
		localSuccess = "Implementation and local checks passed.\nDraft handoff: " + requirement
	)
	for _, tc := range []struct {
		name, answer           string
		change                 bool
		agentErr               error
		retry                  bool
		metadataError          string
		findings               bool
		headMoved              bool
		headMovedAfterEvidence bool
		publishedHeadMoved     bool
		pendingChecks          int
		earlyReady             bool
		readyOnPassingPoll     bool
		readyDuringVerifier    bool
		checkAfterVerification bool
		keepDraft              bool
		watchOutages           int
		evidenceOutage         bool
		checkRollups           []string
		checkRuns              string
		commitStatuses         string
		requiredChecks         string
		ruleset                string
		wantChecksPending      bool
		checksFail             bool
		filesPartial           bool
		filesTruncated         bool
		largePatch             bool
		baseMoved              bool
		baseMovedAfterEvidence bool
		publishedBaseMoved     bool
		mergedAtReview         string
		repair                 bool
		repairHeadLag          int
		repairStillHasFindings bool
		delivery               bool
		noCI                   bool
		operatorMerge          bool
		mergedDuringEvidence   bool
		mergedOnEvidencePoll   bool
		reviewStale            bool
		reviewTruncated        bool
		verifierBlocked        bool
		queued                 bool
		wantExit               int
		wantEvidenceError      string
	}{
		{name: "verify private draft from supplied evidence", answer: localSuccess, change: true, requiredChecks: `{"contexts":["CI"]}`},
		{name: "verify standard handoff from supplied evidence", answer: "Draft handoff: " + draftHandoffStandard, change: true},
		{name: "checks watch recovers within the same run", answer: localSuccess, change: true, watchOutages: 1},
		{name: "checks resume on the published draft", answer: localSuccess, change: true, watchOutages: 3, wantExit: ExitPR},
		{name: "draft evidence resumes with a fresh review on the new head", answer: localSuccess, change: true, evidenceOutage: true, wantExit: ExitPR},
		{name: "standard handoff requires a passing verdict", answer: "Draft handoff: " + draftHandoffStandard, change: true, verifierBlocked: true, wantExit: ExitPR},
		{name: "missing current-head checks", answer: localSuccess, change: true, checkRollups: []string{"null", "null"}, wantExit: ExitPR,
			wantEvidenceError: "missing or invalid check results for the current draft head"},
		{name: "unnamed check result", answer: localSuccess, change: true, checkRollups: []string{`[{"conclusion":"SUCCESS"}]`, `[{"conclusion":"SUCCESS"}]`}, wantExit: ExitPR,
			wantEvidenceError: "unnamed check result for the current draft head"},
		{name: "verify standard handoff with no configured checks", answer: "Draft handoff: " + draftHandoffStandard, change: true, checkRollups: []string{"[]"}},
		{name: "configured check has not reported", answer: localSuccess, change: true, checkRollups: []string{"[]", "[]"}, requiredChecks: `{"contexts":["CI"]}`, wantExit: ExitPR,
			wantEvidenceError: "missing result for configured draft check"},
		{name: "wait for pending checks on reviewed head", answer: localSuccess, change: true, pendingChecks: 1},
		{name: "read repaired head only after disk readmission", answer: localSuccess, change: true, repair: true, pendingChecks: 1},
		{name: "deliver clean review", answer: localSuccess, change: true, delivery: true},
		{name: "deliver clean review without CI", answer: localSuccess, change: true, delivery: true, noCI: true},
		{name: "deliver by observing an operator merge", answer: localSuccess, change: true, delivery: true, operatorMerge: true},
		{name: "deliver repair after head propagation", answer: localSuccess, change: true, repair: true, repairHeadLag: 1, delivery: true},
		{name: "deliver repair after check retry", answer: localSuccess, change: true, repair: true, repairHeadLag: 6, delivery: true, wantExit: ExitPR},
		{name: "stop repairs at captured limit after check retry", answer: localSuccess, change: true, repair: true, repairHeadLag: 6, repairStillHasFindings: true, delivery: true, wantExit: ExitPR},
		{name: "restore early readiness while checks are pending", answer: localSuccess, change: true, earlyReady: true},
		{name: "keep explicit draft after early readiness", answer: localSuccess, change: true, earlyReady: true, keepDraft: true},
		{name: "restore readiness on the last check poll", answer: localSuccess, change: true, earlyReady: true, readyOnPassingPoll: true},
		{name: "restore readiness changed during verification", answer: localSuccess, change: true, readyDuringVerifier: true},
		{name: "readiness starts another required check during verification", answer: localSuccess, change: true, readyDuringVerifier: true, checkAfterVerification: true, wantChecksPending: true, wantExit: ExitPR},
		{name: "empty rollup cannot erase observed pending checks", answer: localSuccess, change: true, pendingChecks: 1, wantExit: ExitPR, wantChecksPending: true,
			checkRollups: []string{`[{"name":"CI","status":"IN_PROGRESS"}]`, `[]`}},
		{name: "empty rollup after pending with required checks", answer: localSuccess, change: true, pendingChecks: 1, wantExit: ExitPR, wantChecksPending: true,
			requiredChecks: `{"contexts":["CI"]}`,
			checkRollups:   []string{`[{"name":"CI","status":"IN_PROGRESS"}]`, `[]`}},
		{name: "successful partial rollup omits required check", answer: localSuccess, change: true, pendingChecks: 1, wantExit: ExitPR, wantChecksPending: true,
			requiredChecks: `{"checks":[{"context":"CI"},{"context":"lint"}]}`,
			checkRollups:   []string{`[{"name":"CI","status":"IN_PROGRESS"}]`, `[{"name":"CI","conclusion":"SUCCESS"}]`}},
		{name: "successful partial rollup omits ruleset check", answer: localSuccess, change: true, pendingChecks: 1, wantExit: ExitPR, wantChecksPending: true,
			ruleset:      `[] [{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"}]}}]`,
			checkRollups: []string{`[{"name":"CI","status":"IN_PROGRESS"}]`, `[{"name":"CI","conclusion":"SUCCESS"}]`}},
		{name: "successful partial rollup omits previously observed check", answer: localSuccess, change: true, pendingChecks: 1, wantExit: ExitPR, wantChecksPending: true,
			checkRollups: []string{`[{"name":"CI","conclusion":"SUCCESS"},{"context":"lint","state":"PENDING"}]`, `[{"name":"CI","conclusion":"SUCCESS"}]`}},
		{name: "successful partial rollup omits optional check run", answer: localSuccess, change: true, wantExit: ExitPR, wantChecksPending: true,
			checkRuns: `{"check_runs":[{"name":"CI"}]} {"check_runs":[{"name":"lint"}]}`},
		{name: "optional commit status appears after partial rollup", answer: localSuccess, change: true, pendingChecks: 1,
			commitStatuses: `{"statuses":[{"context":"lint"}]}`,
			checkRollups:   []string{`[{"name":"CI","conclusion":"SUCCESS"}]`, `[{"name":"CI","conclusion":"SUCCESS"},{"context":"lint","state":"SUCCESS"}]`}},
		{name: "required status context appears after partial rollup", answer: localSuccess, change: true, pendingChecks: 1,
			requiredChecks: `{"contexts":["CI","lint"]}`,
			checkRollups:   []string{`[{"name":"CI","conclusion":"SUCCESS"}]`, `[{"name":"CI","conclusion":"SUCCESS"},{"context":"lint","state":"SUCCESS"}]`}},
		{name: "pending checks outlive the wait budget", answer: localSuccess, change: true, pendingChecks: 3, wantExit: ExitPR},
		{name: "head moves while checks are pending", answer: localSuccess, change: true, pendingChecks: 1, headMoved: true, wantExit: ExitPR},
		{name: "failed checks block verification", answer: localSuccess, change: true, checksFail: true, wantExit: ExitPR},
		{name: "retry verification on the same draft", answer: localSuccess, change: true, retry: true, wantExit: ExitPR},
		{name: "retry rejected assignee on the same draft", answer: localSuccess, change: true, metadataError: "assignee", wantExit: ExitPR},
		{name: "retry rejected label on the same draft", answer: localSuccess, change: true, metadataError: "label", wantExit: ExitPR},
		{name: "findings keep the draft blocked", answer: localSuccess, change: true, findings: true, wantExit: ExitPR},
		{name: "head changed before verification", answer: localSuccess, change: true, headMoved: true, wantExit: ExitPR},
		{name: "head changed during offline verification", answer: localSuccess, change: true, headMovedAfterEvidence: true, wantExit: ExitPR},
		{name: "published head moves while PR metadata is stale", answer: localSuccess, change: true, publishedHeadMoved: true, wantExit: ExitPR},
		{name: "changed-file evidence unavailable", answer: localSuccess, change: true, filesPartial: true, wantExit: ExitPR},
		{name: "changed-file inventory exceeds evidence limit", answer: localSuccess, change: true, filesTruncated: true, wantExit: ExitPR,
			wantEvidenceError: "live changed-file inventory exceeds the draft verification evidence limit"},
		{name: "verify complete large patch offline", answer: localSuccess, change: true, largePatch: true, requiredChecks: `{"contexts":["CI"]}`},
		{name: "base changed after review", answer: localSuccess, change: true, baseMoved: true, wantExit: ExitPR,
			wantEvidenceError: "reviewed patch evidence has a stale draft base"},
		{name: "base changed during offline verification", answer: localSuccess, change: true, baseMovedAfterEvidence: true, wantExit: ExitPR},
		{name: "published base moves while PR metadata is stale", answer: localSuccess, change: true, publishedBaseMoved: true, wantExit: ExitPR},
		{name: "external merge at initial mergeability check", answer: localSuccess, change: true, mergedAtReview: "before"},
		{name: "external merge during final review", answer: localSuccess, change: true, mergedAtReview: "during"},
		{name: "external merge during repair CI", answer: localSuccess, change: true, repair: true, mergedAtReview: "repair checks"},
		{name: "external merge at final mergeability check", answer: localSuccess, change: true, mergedAtReview: "after"},
		{name: "external merge during offline verification", answer: localSuccess, change: true, mergedDuringEvidence: true},
		{name: "external merge while polling for draft checks", answer: localSuccess, change: true, pendingChecks: 1, mergedOnEvidencePoll: true},
		{name: "submitted review stale", answer: localSuccess, change: true, reviewStale: true, wantExit: ExitPR},
		{name: "submitted review body exceeds evidence limit", answer: localSuccess, change: true, reviewTruncated: true, wantExit: ExitPR,
			wantEvidenceError: "submitted review 1 body exceeds the draft verification evidence limit"},
		{name: "blocked implementation", answer: "Failure: Implementation remains blocked.\nError details: Local tests failed.\nDraft handoff: " + requirement, change: true, wantExit: ExitCodex},
		{name: "agent execution failed", answer: localSuccess, change: true, agentErr: errors.New("execution failed"), wantExit: ExitCodex},
		{name: "missing handoff", answer: "Implementation and local checks passed.", change: true, wantExit: ExitCodex},
		{name: "missing handoff with queued verification", answer: "Implementation and local checks passed.", change: true, queued: true, wantExit: ExitCodex},
		{name: "handoff is not final", answer: localSuccess + "\nImplementation complete.", change: true, wantExit: ExitCodex},
		{name: "empty handoff", answer: "Draft handoff:", change: true, wantExit: ExitCodex},
		{name: "no change", answer: "No changes needed."},
		{name: "handoff without changes", answer: localSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			remote := filepath.Join(root, "remote.git")
			seed := filepath.Join(root, "seed")
			if err := os.Mkdir(seed, 0o700); err != nil {
				t.Fatal(err)
			}
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := gitCommand(dir, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
				res, err := (execx.OSRunner{}).Run(t.Context(), cmd)
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, res.Stderr)
				}
				return strings.TrimSpace(res.Stdout)
			}
			git(root, "init", "--bare", "-b", "main", remote)
			git(seed, "init", "-b", "main")
			if err := os.WriteFile(filepath.Join(seed, "work.txt"), []byte("base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(seed, "add", "work.txt")
			git(seed, "commit", "-m", "base")
			git(seed, "branch", branch)
			git(seed, "push", remote, "main", branch)
			base := git(seed, "rev-parse", "HEAD")

			api := &reviewAPIStub{t: t}
			wantRequirement, _ := draftHandoffRequest(tc.answer)
			var implementations, creates, verifications, ready, evidenceReads, waits int
			var watchReads, evidenceOutages int
			var draftRestorations, reviewerRuns, verificationPolicyReads int
			var repairs, repairHeadReads, merges int
			var headBeforeRepair string
			var merged, mergeChecksReady bool
			workspaceHeld := true
			mergeSeenDuringRepair := false
			var lastWait string
			var draft, accepted, metadataRetry bool
			savedResume := ""
			labels := []string{"existing label"}
			assignees := []string{"creator"}
			var savedRequirement string
			var artifactPath string
			liveBase := base
			hasGitHubCredential := func(cmd execx.Command) bool {
				if cmd.Env == nil {
					return os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != ""
				}
				return slices.ContainsFunc(cmd.Env, func(entry string) bool {
					key, _, _ := strings.Cut(entry, "=")
					return slices.Contains(credentialKeys, key)
				})
			}
			now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
			var timing strings.Builder
			runner := issueBranchRunner(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				if len(cmd.Args) > 1 && cmd.Name == "gh" && cmd.Args[0] == "api" && cmd.Args[1] == "--include" {
					cmd.Args = append([]string{"api"}, cmd.Args[2:]...)
				}
				args := strings.Join(cmd.Args, " ")
				if tc.earlyReady && cmd.Name == "gh" && (strings.HasPrefix(args, "pr merge ") || slices.Contains(cmd.Args, "repos/acme/repo/pulls/42/merge")) {
					t.Fatal("workflow without auto-merge attempted a merge")
				}
				if cmd.Name == "gh" && strings.Contains(args, "--method POST repos/acme/repo/issues/42/") && savedResume != prURL {
					t.Fatal("metadata mutation preceded saved retry target")
				}
				if res, ok, err := api.handle(cmd); ok {
					return res, err
				}
				switch cmd.Name {
				case "git":
					if tc.repair && strings.HasPrefix(args, "ls-remote ") && !workspaceHeld {
						t.Fatal("checkout-backed Git command ran without workspace admission")
					}
					if args == "remote get-url origin" {
						return execx.Result{Stdout: repoURL}, nil
					}
					if i := slices.Index(cmd.Args, repoURL); i >= 0 {
						cmd.Args = slices.Clone(cmd.Args)
						cmd.Args[i] = remote
					}
					cmd.Args = append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, cmd.Args...)
					res, err := (execx.OSRunner{}).Run(ctx, cmd)
					if err == nil && tc.repair && repairs > 0 && strings.HasPrefix(args, "push ") && !slices.Contains(cmd.Args, "--dry-run") {
						head := git(remote, "rev-parse", branch)
						if head != api.head {
							headBeforeRepair, api.head = api.head, head
						}
						git(remote, "update-ref", "refs/pull/42/head", api.head)
					}
					return res, err
				case "codex":
					if cmd.Stdin != "" {
						now = now.Add(2 * time.Second)
					}

					switch {
					case args == "--help":
						return execx.Result{}, nil
					case args == "--version":
						return execx.Result{Stdout: "codex-cli test"}, nil
					case strings.Contains(cmd.Stdin, "Verify the requested draft-handoff criteria"):
						verifications++
						if tc.earlyReady && (evidenceReads != 3 || draftRestorations != 1) {
							t.Error("verification must wait for restored draft and passing checks")
						}
						if hasGitHubCredential(cmd) || !strings.Contains(args, "network={enabled=false}") || !slices.Contains(cmd.Args, `web_search="disabled"`) {
							t.Error("private draft verifier must run without GitHub credentials or network access")
						}
						if !draft || api.review.Head != api.head || !strings.Contains(cmd.Stdin, wantRequirement) ||
							!strings.Contains(cmd.Stdin, `"state":"OPEN","isDraft":true`) ||
							!strings.Contains(cmd.Stdin, `"headRefOid":"`+api.head+`"`) ||
							!strings.Contains(cmd.Stdin, `"commit_id":"`+api.head+`"`) ||
							!strings.Contains(cmd.Stdin, api.review.URL) || !strings.Contains(cmd.Stdin, "No material issues found.") ||
							(!tc.noCI && len(tc.checkRollups) == 0 && !strings.Contains(cmd.Stdin, `"conclusion":"SUCCESS"`)) || !strings.Contains(cmd.Stdin, `"path":"work.txt"`) ||
							(!tc.largePatch && !strings.Contains(cmd.Stdin, "+implemented")) {
							t.Error("verification started before draft and submitted evidence were available")
						}
						if tc.largePatch {
							_, ref, _ := strings.Cut(cmd.Stdin, "Read the complete patch in the read-only file ")
							quotedPath, _, _ := strings.Cut(ref, ". Read it")
							var err error
							artifactPath, err = strconv.Unquote(quotedPath)
							if err != nil {
								t.Fatal(err)
							}
							artifact, err := os.ReadFile(artifactPath)
							if err != nil {
								t.Fatal(err)
							}
							expected, err := (execx.OSRunner{}).Run(ctx, gitCommand(cmd.Dir, "diff", "--binary", base+"..."+api.head))
							if err != nil || string(artifact) != expected.Stdout || len(artifact) <= 55000 ||
								!strings.Contains(string(artifact), "+last line of evidence\n") || strings.Contains(cmd.Stdin, "+implemented") ||
								!strings.Contains(cmd.Stdin, fmt.Sprintf("SHA-256: %x", sha256.Sum256(artifact))) {
								t.Errorf("offline artifact does not contain the exact reviewed patch (%d bytes): %v", len(artifact), err)
							}
							info, err := os.Stat(artifactPath)
							if err != nil || info.Mode().Perm() != 0o400 {
								t.Errorf("reviewed patch is not read-only: %v, %v", info, err)
							}
						}
						if !strings.Contains(cmd.Stdin, "Do not use gh, GitHub APIs, browser, web search") ||
							strings.Contains(cmd.Stdin, "AGENTS.md") || strings.Contains(cmd.Stdin, "Backpressure") {
							t.Error("verification prompt must be self-contained and forbid live lookups")
						}
						if tc.requiredChecks == `{"contexts":["CI"]}` && !strings.Contains(cmd.Stdin, "Configured CI check names:\n[\"CI\"]") {
							t.Error("verification prompt omitted configured CI checks")
						}
						if slices.Equal(tc.checkRollups, []string{"[]"}) && !strings.Contains(cmd.Stdin, "Configured CI check names:\n[]") {
							t.Error("verification prompt omitted the empty configured check list")
						}
						if tc.retry && verifications == 1 {
							return execx.Result{Stdout: `{"status":"blocked","evidence":"The requested change needs additional evidence"}`}, nil
						}
						if tc.verifierBlocked {
							return execx.Result{Stdout: `{"status":"blocked","evidence":"The patch conflicts with the changed-file inventory"}`}, nil
						}
						if tc.headMovedAfterEvidence && verifications == 1 {
							api.head = "new-head"
						}
						if tc.baseMovedAfterEvidence {
							liveBase = "different-base"
						}
						if tc.publishedBaseMoved {
							git(seed, "commit", "--allow-empty", "-m", "advance draft base")
							git(seed, "push", remote, "main")
						}
						if tc.publishedHeadMoved {
							git(remote, "update-ref", "refs/heads/"+branch, base)
						}
						if tc.readyDuringVerifier {
							draft = false
						}
						return execx.Result{Stdout: `{"status":"passed","evidence":"The live inventory lists work.txt, its patch shows the change, and CI and the clean review match the draft head."}`}, nil
					case tc.repair && strings.Contains(cmd.Stdin, "Automated review repair pass"):
						repairs++
						return execx.Result{Stdout: "Fixed."}, os.WriteFile(filepath.Join(cmd.Dir, "work.txt"), []byte("implemented with repair\n"), 0o600)
					case strings.Contains(cmd.Stdin, "Prepared pull-request review context"):
						reviewerRuns++
						if !draft {
							t.Error("review ran before draft publication")
						}
						wantRange := fmt.Sprintf("- Local merge-base diff range: %q", base+"..."+api.head)
						if !strings.Contains(cmd.Stdin, `- Local base branch ref: "refs/remotes/origin/main"`) || !strings.Contains(cmd.Stdin, wantRange) {
							t.Errorf("draft reviewer did not receive the fetched base ref and commit range %s", wantRange)
						}
						if tc.findings || tc.repairStillHasFindings || (tc.repair && reviewerRuns == 1) {
							return execx.Result{Stdout: `{"status":"findings","findings":[{"severity":"high","path":"work.txt","line":1,"title":"Incorrect output"}]}`}, nil
						}
						return execx.Result{Stdout: cleanReviewOutput}, nil
					default:
						implementations++
						if !strings.Contains(cmd.Stdin, draftHandoffInstructions) {
							t.Error("implementation prompt omitted draft handoff contract")
						}
						if tc.change {
							content := "implemented\n"
							if tc.largePatch {
								content += strings.Repeat("extra line\n", 5000) + "last line of evidence\n"
							}
							if err := os.WriteFile(filepath.Join(cmd.Dir, "work.txt"), []byte(content), 0o600); err != nil {
								return execx.Result{}, err
							}
						}
						return execx.Result{Stdout: tc.answer}, tc.agentErr
					}
				case "gh":
					switch {
					case tc.delivery && strings.HasPrefix(args, "api --paginate repos/acme/repo/issues?"):
						if merged {
							return execx.Result{Stdout: "[]"}, nil
						}
						return execx.Result{Stdout: `[{"number":17,"title":"Issue","html_url":"https://github.com/acme/repo/issues/17"}]`}, nil
					case tc.delivery && strings.Contains(args, "closingIssuesReferences"):
						return execx.Result{Stdout: "0"}, nil
					case tc.delivery && strings.Contains(args, "autoMergeRequest{enabledAt}"):
						if !merged {
							return execx.Result{Stdout: `{"state":"OPEN","autoMergeRequest":null,"mergeQueueEntry":null}`}, nil
						}
						return execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-11-25T12:00:00Z"}`}, nil
					case tc.delivery && strings.HasPrefix(args, "api --method PUT repos/acme/repo/pulls/42/merge "):
						if !mergeChecksReady || merged || draft || ready != 1 || verifications != 1 || api.review.Head != api.head || !slices.Contains(cmd.Args, "sha="+api.head) {
							t.Fatal("merge must follow draft verification, readiness, and clean review of the published head")
						}
						merges++
						merged = true
						return execx.Result{Stdout: `{"merged":true}`}, nil
					case slices.Equal(cmd.Args, []string{"api", "repos/acme/repo/pulls/42", "--jq", draftPatchCommitsQuery}):
						if !hasGitHubCredential(cmd) {
							return execx.Result{}, errors.New("private draft commit metadata requires authentication")
						}
						if tc.baseMoved {
							liveBase = "different-base"
						}
						return execx.Result{Stdout: fmt.Sprintf(`{"base":%q,"head":%q,"baseRef":"main","headRef":%q,"state":"open","merged":%t,"draft":%t}`,
							liveBase, api.head, branch, tc.mergedDuringEvidence && verifications > 0, draft)}, nil
					case args == fmt.Sprintf("api repos/acme/repo/pulls/42/reviews/%d", api.review.ID):
						if !hasGitHubCredential(cmd) {
							return execx.Result{}, errors.New("private draft review requires authentication")
						}
						review := api.review
						if tc.reviewStale {
							review.Head = "older-head"
						}
						if tc.reviewTruncated {
							const evidence = "\nAdditional review evidence."
							review.Body += strings.Repeat(evidence, maxReviewCommentsChars/len(evidence)+1)
						}
						data, err := json.Marshal(review)
						return execx.Result{Stdout: string(data)}, err
					case args == "--version" || strings.HasPrefix(args, "auth ") || strings.HasPrefix(args, "issue edit "):
						return execx.Result{}, nil
					case strings.HasPrefix(args, "pr create "):
						creates++
						if !slices.Contains(cmd.Args, "--draft") || !accepted || savedRequirement != wantRequirement {
							t.Error("harness published before accepting the verification checkpoint as a draft")
						}
						draft = slices.Contains(cmd.Args, "--draft")
						api.head = git(remote, "rev-parse", branch)
						if api.head == base {
							t.Error("draft opened before implementation was committed and pushed")
						}
						git(remote, "update-ref", "refs/pull/42/head", api.head)
						return execx.Result{Stdout: prURL}, nil
					case strings.HasPrefix(args, "api --method POST repos/acme/repo/issues/42/"):
						if !draft || creates != 1 {
							t.Fatal("routing must target the published draft")
						}
						var body map[string][]string
						if err := json.Unmarshal([]byte(cmd.Stdin), &body); err != nil {
							t.Fatal(err)
						}
						values := &labels
						switch {
						case strings.Contains(args, "/assignees "):
							values = &assignees
							if tc.metadataError != "assignee" || metadataRetry {
								if !slices.Contains(*values, body["assignees"][0]) {
									*values = append(*values, body["assignees"][0])
								}
							}
						case strings.Contains(args, "/labels "):
							label := body["labels"][0]
							if tc.metadataError == "label" && !metadataRetry && label == "priority, high" {
								return execx.Result{Stderr: "HTTP 422: label rejected"}, errors.New("exit status 1")
							}
							if !slices.Contains(*values, label) {
								*values = append(*values, label)
							}
						default:
							t.Fatalf("unexpected metadata endpoint: %s", args)
						}
						data, err := json.Marshal(*values)
						return execx.Result{Stdout: string(data)}, err
					case strings.HasPrefix(args, "pr ready "):
						if !slices.Contains(cmd.Args, "--undo") {
							ready++
							if !draft || verifications == 0 {
								t.Error("draft released before verification")
							}
						} else if !draft {
							draftRestorations++
						}
						draft = slices.Contains(cmd.Args, "--undo")
						return execx.Result{}, nil
					case args == "api repos/acme/repo/branches/main/protection/required_status_checks":
						if tc.noCI {
							return execx.Result{Stdout: `{"contexts":[]}`}, nil
						}
						if tc.earlyReady && api.posts > 0 {
							verificationPolicyReads++
							return execx.Result{Stdout: `{"contexts":["CI","Web and browser tests"]}`}, nil
						}
						if api.posts == 0 {
							return execx.Result{Stdout: `{"contexts":["CI"]}`}, nil
						}
						if tc.requiredChecks != "" {
							return execx.Result{Stdout: tc.requiredChecks}, nil
						}
						return execx.Result{Stderr: "Branch not protected"}, errors.New("exit status 1")
					case args == "api --paginate repos/acme/repo/rules/branches/main?per_page=100":
						if tc.ruleset != "" && api.posts > 0 {
							return execx.Result{Stdout: tc.ruleset}, nil
						}
						return execx.Result{Stdout: "[]"}, nil
					case strings.HasPrefix(args, "api graphql -f id=PR_id "):
						if tc.noCI {
							return execx.Result{Stdout: checkRollupPageJSON(branch, api.head, api.head, "[]", false, "")}, nil
						}
						return execx.Result{Stdout: checkRollupPageJSON(branch, api.head, api.head, `[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`, false, "")}, nil
					case args == "api --paginate repos/acme/repo/commits/"+api.head+"/check-runs?per_page=100":
						if tc.checkRuns != "" {
							return execx.Result{Stdout: tc.checkRuns}, nil
						}
						return execx.Result{Stdout: `{"check_runs":[]}`}, nil
					case args == "api --paginate repos/acme/repo/commits/"+api.head+"/status?per_page=100":
						if tc.commitStatuses != "" {
							return execx.Result{Stdout: tc.commitStatuses}, nil
						}
						return execx.Result{Stdout: `{"statuses":[]}`}, nil
					case slices.Equal(cmd.Args, append(workflowDispatchRunsCommand("", branch).Args, "--repo", "acme/repo", "--commit", api.head)):
						return execx.Result{Stdout: "[]"}, nil
					case strings.HasPrefix(args, "pr checks "):
						watchReads++
						if tc.noCI {
							return execx.Result{Stderr: "no required checks reported"}, errors.New("exit status 1")
						}
						if watchReads <= tc.watchOutages {
							return execx.Result{Stderr: "HTTP 503: service unavailable"}, errors.New("exit status 1")
						}
						return execx.Result{Stdout: "CI\tpass\n"}, nil
					case strings.HasPrefix(args, "pr view "):
						head := api.head
						state := "OPEN"
						if merged {
							state = "MERGED"
						}
						if tc.repairHeadLag > 0 && headBeforeRepair != "" && slices.Equal(cmd.Args, prStateViewCommand("", prURL).Args) {
							repairHeadReads++
							if repairHeadReads <= tc.repairHeadLag {
								head = headBeforeRepair
							}
						}
						if slices.Contains(cmd.Args, "state,mergeable,baseRefName,headRefName") &&
							(tc.mergedAtReview == "before" || (tc.mergedAtReview == "after" && api.posts > 0)) {
							state = "MERGED"
						}
						if tc.mergedAtReview == "during" && reviewerRuns > 0 &&
							slices.Contains(cmd.Args, "url,title,state,mergedAt,baseRefName,headRefName,headRefOid,headRepository,headRepositoryOwner") {
							state = "MERGED"
						}
						if tc.mergedAtReview == "repair checks" && repairs > 0 && watchReads > 1 {
							mergeSeenDuringRepair = true
							state = "MERGED"
						}
						fileEvidence := ""
						rollup := `[{"name":"CI","conclusion":"SUCCESS"}]`
						if tc.noCI {
							rollup = "[]"
						}
						if strings.Contains(args, "statusCheckRollup") && strings.Contains(args, "changedFiles,files") {
							if tc.evidenceOutage && evidenceOutages < 3 {
								evidenceOutages++
								return execx.Result{Stderr: "Post https://api.github.com/graphql: unexpected EOF"}, errors.New("exit status 1")
							}
							if !hasGitHubCredential(cmd) {
								return execx.Result{}, errors.New("private draft file metadata requires authentication")
							}
							evidenceReads++
							if tc.mergedOnEvidencePoll && evidenceReads > tc.pendingChecks {
								state = "MERGED"
							}
							switch {
							case tc.checkAfterVerification && verifications > 0:
								rollup = `[{"name":"CI","conclusion":"SUCCESS"},{"name":"Web and browser tests","status":"IN_PROGRESS"}]`
							case tc.earlyReady && (evidenceReads == 1 || (!tc.readyOnPassingPoll && evidenceReads == 2)):
								rollup = `[{"name":"CI","conclusion":"SUCCESS"},{"name":"Web and browser tests","status":"IN_PROGRESS"}]`
							case tc.earlyReady:
								rollup = `[{"name":"CI","conclusion":"SUCCESS"},{"name":"Web and browser tests","conclusion":"SUCCESS"}]`
							case evidenceReads <= len(tc.checkRollups):
								rollup = tc.checkRollups[evidenceReads-1]
							case len(tc.checkRollups) > 0:
								rollup = tc.checkRollups[len(tc.checkRollups)-1]
							case tc.pendingChecks >= evidenceReads:
								rollup = `[{"name":"CI","status":"IN_PROGRESS"}]`
							case tc.checksFail:
								rollup = `[{"name":"CI","conclusion":"FAILURE"}]`
							}
							if tc.headMoved && api.posts == 1 && evidenceReads > tc.pendingChecks {
								head = "new-head"
							}
							additions := 1
							if tc.largePatch {
								additions = 5002
							}
							files := []string{fmt.Sprintf(`{"path":"work.txt","additions":%d,"deletions":1}`, additions)}
							if tc.filesTruncated {
								for i := range 80 {
									path := fmt.Sprintf("%sextra-%d.txt", strings.Repeat("directory/", 20), i)
									files = append(files, fmt.Sprintf(`{"path":%q,"additions":1,"deletions":0}`, path))
								}
							}
							fileCount := len(files)
							if tc.filesPartial {
								fileCount++
							}
							fileEvidence = fmt.Sprintf(`,"changedFiles":%d,"files":[%s]`, fileCount, strings.Join(files, ","))
						}
						mergeState := "CLEAN"
						if tc.delivery && strings.Contains(args, "statusCheckRollup,mergeStateStatus") && !mergeChecksReady {
							if tc.noCI {
								mergeState = "BLOCKED"
							} else {
								rollup = `[{"name":"CI","status":"IN_PROGRESS"}]`
							}
						}
						return execx.Result{Stdout: fmt.Sprintf(`{"id":"PR_id","number":42,"url":%q,"state":%q,"mergeable":"MERGEABLE","mergeStateStatus":%q,"baseRefName":"main","headRefName":%q,"headRefOid":%q,"isDraft":%t,"statusCheckRollup":%s%s}`, prURL, state, mergeState, branch, head, draft, rollup, fileEvidence)}, nil
					case strings.Contains(args, "api repos/acme/repo/pulls?") && creates > 0:
						return execx.Result{Stdout: fmt.Sprintf(`[{"html_url":%q,"title":"Issue","state":"open","head":{"ref":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main"}}]`, prURL, branch)}, nil
					case strings.HasPrefix(args, "api "):
						return execx.Result{Stdout: "[]"}, nil
					}
				}
				return execx.Result{}, fmt.Errorf("unexpected command %s %s", cmd.Name, args)
			})
			h := New(runner)
			const verificationEstimate = 30 * time.Second
			const verificationTimingKey = "acme/repo/checks"
			h.CheckTimings = &CheckTimings{}
			h.CheckTimings.record(verificationTimingKey, verificationEstimate)
			h.ReleaseWorkspace = func() { workspaceHeld = false }
			h.AcquireWorkspace = func(context.Context) error {
				if mergeSeenDuringRepair {
					t.Fatal("repair cycle requested disk after observing a merge")
				}
				workspaceHeld = true
				return nil
			}
			h.PRChecksWatchTimeout = 10 * time.Second
			if tc.earlyReady && !tc.readyOnPassingPoll {
				h.PRChecksWatchTimeout = 20 * time.Second
			}
			h.Logf = func(format string, args ...any) {
				if line := fmt.Sprintf(format, args...); strings.Contains(line, "action=wait_for_reviewed_head_evidence") {
					lastWait = line
				}
			}
			h.Sleep = func(_ context.Context, delay time.Duration) error {
				now = now.Add(delay)
				waits++
				if tc.earlyReady {
					if !accepted || !draft || api.posts != 1 || reviewerRuns != 1 || ready != 0 || verifications != 0 {
						t.Fatal("original claim must keep waiting in draft after one review without release or verification")
					}
					if !strings.Contains(lastWait, "stage=pr_verification status=waiting") ||
						!strings.Contains(lastWait, `pending_checks="Web and browser tests"`) ||
						!strings.Contains(lastWait, fmt.Sprintf("reviewed_head=%q", api.head)) {
						t.Fatalf("missing in-place check progress: %s", lastWait)
					}
					_, refreshed, _ := strings.Cut(lastWait, "last_github_refresh=")
					if _, err := time.Parse(time.RFC3339, strings.Trim(refreshed, `"`)); err != nil {
						t.Fatalf("invalid GitHub refresh in progress: %s", lastWait)
					}
					if waits == 1 {
						if !tc.readyOnPassingPoll && (delay <= 14*time.Second || delay > 15*time.Second) {
							t.Fatalf("draft poll delay = %v, want about 15s from the learned CI estimate", delay)
						}
						draft = false
					}
				}
				return nil
			}
			t.Cleanup(func() {
				if got := h.CheckTimings.average(verificationTimingKey); got != verificationEstimate {
					t.Errorf("draft evidence wait changed build estimate to %v, want %v", got, verificationEstimate)
				}
			})
			h.SaveWorkflowPRResume = func(url string) error { savedResume = url; return nil }
			h.Workspace = workspace.Manager{Root: root, NewGUID: func() string { return "issue-workspace" }}
			if tc.queued {
				h.IssuePRVerification = requirement
				savedRequirement = requirement
			}
			h.SaveIssuePublicationReady = func(ready bool, verification string) error {
				accepted = ready
				if verification != "" {
					savedRequirement = verification
				}
				return nil
			}
			cfg := sampleConfig()
			cfg.Draft = tc.keepDraft
			cfg.RepoURL, cfg.TargetSubdir = repoURL, ""
			cfg.GitHubIssue = &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, URL: "https://github.com/acme/repo/issues/17",
				Claim: &config.GitHubIssueClaim{ID: "claim", Branch: branch, BaseBranch: "main", ExclusionLabel: "claimed", Attempt: 1}}
			if tc.metadataError != "" {
				cfg.Workflow = &config.WorkflowRunConfig{ID: "route", EventID: "route-event", PRAssignee: "maintainer", PRLabels: []string{"tracking", "priority, high"}}
			}
			if tc.largePatch {
				h.FinalReviewPasses = 10
				cfg.Workflow = &config.WorkflowRunConfig{ID: "delivery", EventID: "delivery-event", ReviewAgent: config.ReviewAgentConfig{Complexity: "easy"}}
			}
			if tc.repair {
				h.FinalReviewPasses = 1
			}
			var delivery *workflow.Engine
			var activation workflow.Run
			var dispatches, mergeReceipts int
			if tc.delivery {
				h.ResumeIssueWorkspace = true
				h.IssueWorkspaceDir = filepath.Join(root, "issue-workspace")
				passes := 10
				if tc.repair {
					passes = 1
				}
				delivery = newIssueDeliveryTestEngine(t, root, runner, passes, func(run workflow.Run) {
					dispatches++
					activation = run
				})
				delivery.RecordIssueMerge = func(run workflow.Run, pr workflow.Item, byApp bool) error {
					mergeReceipts++
					if !merged || !pr.Merged || byApp == tc.operatorMerge || run.ID != activation.ID || run.RequestID != "issue-task" || pr.URL != prURL {
						t.Fatalf("bad merge receipt: run=%s, PR=%+v, byApp=%t", run.ID, pr, byApp)
					}
					return nil
				}
				if err := delivery.Tick(t.Context()); err != nil || dispatches != 1 {
					t.Fatalf("issue dispatches=%d: %v", dispatches, err)
				}
				policy := activation.ReviewPolicy()
				cfg.Workflow = &config.WorkflowRunConfig{ID: activation.Workflow.ID, EventID: activation.ID,
					DefaultApplied: true, PolicyHash: activation.Workflow.ExecutionPolicyHash(),
					ReviewCycles: policy.ReviewCycles, ReviewAgent: policy.ReviewAgent}
			}
			timingCtx := tasktiming.WithRecorder(t.Context(), "task", "attempt", func() time.Time { return now },
				func(format string, args ...any) { fmt.Fprintf(&timing, format+"\n", args...) })
			tasktiming.Set(timingCtx, tasktiming.App)
			result := h.Run(timingCtx, cfg)
			tasktiming.Set(timingCtx, "")
			if tc.repair && tc.wantExit == ExitSuccess && tc.mergedAtReview == "" {
				spans, err := tasktiming.Read(t.Context(), strings.NewReader(timing.String()), "task")
				if err != nil {
					t.Fatal(err)
				}
				order := []tasktiming.Stage{tasktiming.Implementation, tasktiming.Publication, tasktiming.Checks,
					tasktiming.Review, tasktiming.Repair, tasktiming.Publication, tasktiming.Checks, tasktiming.Review,
					tasktiming.Verification, tasktiming.Checks, tasktiming.Verification, tasktiming.Ready}
				next := 0
				var ciMS, agentMS int64
				for _, span := range spans {
					if next < len(order) && span.Stage == order[next] {
						next++
					}
					if span.Stage == tasktiming.Checks {
						if span.AgentMS != nil {
							t.Fatalf("CI wait counted as agent execution:\n%s", timing.String())
						}
						ciMS += *span.WallMS
					}
					if span.AgentMS != nil {
						agentMS += *span.AgentMS
					}
				}
				if next != len(order) || ciMS < 1000 || agentMS != 10000 {
					t.Fatalf("timing: stage sequence matched %d/%d, CI %d ms, agent %d ms\n%s", next, len(order), ciMS, agentMS, timing.String())
				}
			}
			if result.ExitCode != tc.wantExit {
				t.Fatalf("issue run exit = %d, want %d, repair head reads = %d: %v", result.ExitCode, tc.wantExit, repairHeadReads, result.Err)
			}
			if !tc.change || tc.wantExit == ExitCodex {
				if creates != 0 || accepted || git(remote, "rev-parse", branch) != base || result.NoChanges != !tc.change {
					t.Fatalf("unaccepted work was published or no-change result lost: %+v", result)
				}
				return
			}
			keepReceipt := errors.Is(result.Err, errDraftChecksPending) || errors.Is(result.Err, errDraftRestoring)
			if tc.wantExit != ExitSuccess && (!draft || ready != 0 || (result.ReviewReceipt != nil) != keepReceipt || result.PRVerificationPassed) {
				t.Fatalf("failed verification or review released the draft: %+v", result)
			}
			repairRetry := tc.repairHeadLag == 6
			checksRetry := tc.watchOutages == 3 || tc.evidenceOutage || repairRetry
			if checksRetry {
				wantPosts := 0
				if tc.evidenceOutage {
					wantPosts = 1
				}
				wantWaits := 2
				if repairRetry {
					wantPosts, wantWaits = 1, 5
					if repairs != 1 || reviewerRuns != 1 || watchReads != 1 {
						t.Fatalf("stale repair metadata triggered extra work: review=%d repair=%d check=%d", reviewerRuns, repairs, watchReads)
					}
					if err := delivery.Complete(activation.ID, workflow.Outcome{Status: workflow.OutcomeChecksPending, PRURL: result.PRURL}); err != nil {
						t.Fatal(err)
					}
					if err := delivery.Tick(t.Context()); err != nil {
						t.Fatal(err)
					}
					runs, err := delivery.Runs()
					if err != nil || len(runs) != 1 || runs[0].ID != activation.ID || runs[0].Status != workflow.OutcomeChecksPending || merges != 0 {
						t.Fatalf("check wait lost activation or merged: runs=%+v, err=%v", runs, err)
					}
				}
				if !errors.Is(result.Err, ErrChecksUnavailable) || result.PRURL != prURL || waits != wantWaits || api.posts != wantPosts || verifications != 0 {
					t.Fatalf("check query exhaustion did not retain the draft: waits=%d reviews=%d verifier=%d result=%+v", waits, api.posts, verifications, result)
				}
				h.IssueChecksPRURL = result.PRURL
			}
			if tc.verifierBlocked {
				if creates != 1 || api.posts != 1 || verifications != 1 || !strings.Contains(fmt.Sprint(result.Err), "The patch conflicts with the changed-file inventory") {
					t.Fatalf("expected the offline verifier verdict to keep the draft gated: %+v", result)
				}
				return
			}
			if tc.wantEvidenceError != "" {
				if creates != 1 || api.posts != 1 || verifications != 0 || !strings.Contains(fmt.Sprint(result.Err), tc.wantEvidenceError) {
					t.Fatalf("expected evidence validation to block verification with an actionable reason: %+v", result)
				}
				if !strings.Contains(tc.wantEvidenceError, "exceeds") && !errors.Is(result.Err, ErrReviewPending) {
					t.Fatalf("missing draft evidence must leave review pending: %+v", result)
				}
				return
			}
			if tc.filesPartial {
				if creates != 1 || api.posts != 1 || verifications != 0 || !errors.Is(result.Err, ErrReviewPending) || !strings.Contains(fmt.Sprint(result.Err), "missing or incomplete changed-file inventory for the current draft head") {
					t.Fatalf("expected one blocked verification with an actionable reason: %+v", result)
				}
				return
			}
			if tc.pendingChecks > 0 && waits != 1 {
				t.Fatalf("waits = %d, want one pending check retry", waits)
			}
			checksPending := tc.wantChecksPending || tc.pendingChecks > 1
			if checksPending && !errors.Is(result.Err, errDraftChecksPending) {
				t.Fatalf("expected pending verification after retry budget, got %v", result.Err)
			}
			if checksPending && !errors.Is(result.Err, ErrChecksUnavailable) {
				t.Fatalf("pending verification must retain the same issue task: %v", result.Err)
			}
			if checksPending && !tc.headMoved && (result.ReviewReceipt == nil || result.ReviewReceipt.Head != api.head) {
				t.Fatalf("pending verification lost the clean review receipt: %+v", result)
			}
			if tc.checkAfterVerification {
				if verifications != 1 || ready != 0 || !draft || reviewerRuns != 1 {
					t.Fatalf("new check released the draft or repeated review: verify=%d ready=%d draft=%t review=%d", verifications, ready, draft, reviewerRuns)
				}
				return
			}
			if tc.checksFail && !strings.Contains(fmt.Sprint(result.Err), "checks failed on the reviewed pull request head") {
				t.Fatalf("expected failed checks to block verification, got %v", result.Err)
			}
			if tc.headMovedAfterEvidence || tc.publishedHeadMoved {
				if creates != 1 || api.posts != 1 || verifications != 1 || !errors.Is(result.Err, ErrReviewHeadChanged) {
					t.Fatalf("head movement after collecting evidence must invalidate the verified review: %+v", result)
				}
				return
			}
			if tc.baseMovedAfterEvidence || tc.publishedBaseMoved {
				if verifications != 1 || !errors.Is(result.Err, ErrReviewPending) || !strings.Contains(result.Err.Error(), "stale draft base") {
					t.Fatalf("base movement during verification must keep the draft gated: %+v", result)
				}
				return
			}
			if tc.mergedAtReview != "" || tc.mergedDuringEvidence || tc.mergedOnEvidencePoll {
				wantPosts := 1
				if tc.mergedAtReview == "before" || tc.mergedAtReview == "during" {
					wantPosts = 0
				}
				wantVerifications := 0
				if tc.mergedDuringEvidence {
					wantVerifications = 1
				}
				if workspaceHeld || creates != 1 || api.posts != wantPosts || verifications != wantVerifications || ready != 0 ||
					result.Err != nil || result.PRURL != prURL || !result.PRAlreadyMerged || result.ReviewInterrupted || result.PRVerificationPassed {
					t.Fatalf("external merge must complete the task and release its workspace: held=%t, verifications=%d, ready=%d, result=%+v",
						workspaceHeld, verifications, ready, result)
				}
				return
			}
			if tc.findings || tc.headMoved || tc.reviewStale || tc.checksFail || checksPending {
				if creates != 1 || api.posts != 1 || verifications != 0 {
					t.Fatal("expected one submitted review and verification blocked")
				}
				if tc.headMoved && (!errors.Is(result.Err, ErrReviewHeadChanged) || !strings.Contains(result.Err.Error(), "draft head changed after review")) {
					t.Fatalf("expected head movement to require fresh review and checks: %+v", result)
				}
				if tc.reviewStale && (!errors.Is(result.Err, ErrReviewPending) || !strings.Contains(result.Err.Error(), "submitted review 1 to show a clean verdict on the draft head")) {
					t.Fatalf("expected the stale review to block verification: %+v", result)
				}
				if !tc.headMoved {
					return
				}
			}
			if tc.headMoved || tc.evidenceOutage {
				// Advance the real branch before recovery. The earlier clean review
				// must not be replayed against the new remote commit.
				git(seed, "fetch", remote, branch)
				git(seed, "checkout", "-B", branch, "FETCH_HEAD")
				if err := os.WriteFile(filepath.Join(seed, "work.txt"), []byte("implemented\nfresh head\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				git(seed, "commit", "-am", "advance draft head")
				git(seed, "push", remote, branch)
				api.head = git(remote, "rev-parse", branch)
				git(remote, "update-ref", "refs/pull/42/head", api.head)
			}
			wantReviews := 1
			if tc.headMoved || tc.evidenceOutage || tc.repair {
				wantReviews = 2
			}
			wantVerifications := 1
			if tc.metadataError != "" {
				if result.PRURL != prURL || !errors.Is(result.Err, ErrWorkflowPRMetadata) ||
					!strings.Contains(result.Err.Error(), tc.metadataError+` "`) || verifications != 0 {
					t.Fatalf("expected failed %s update on the original draft: %+v", tc.metadataError, result)
				}
				cfg.Workflow.ResumePRURL = result.PRURL
				metadataRetry = true
			}
			if tc.retry || metadataRetry || tc.headMoved || checksRetry {
				h.ResumeIssueWorkspace = true
				h.IssueCheckoutComplete = true
				h.IssuePublicationReady = accepted
				h.IssuePRVerification = savedRequirement
				if !checksRetry {
					cfg.GitHubIssue.Claim.Attempt++
				}
				if repairRetry {
					h.Workspace.NewGUID = func() string { return "retry-run" }
				}
				result = h.Run(t.Context(), cfg)
				if tc.retry {
					wantVerifications++
				}
			}
			if tc.repairStillHasFindings {
				if !errors.Is(result.Err, errFinalReviewFindingsRemain) || !draft || ready != 0 || verifications != 0 || merges != 0 ||
					creates != 1 || implementations != 1 || repairs != 1 || reviewerRuns != 2 || api.posts != 2 || result.PRURL != prURL {
					t.Fatalf("exhausted repair left its draft or ran more work: repairs=%d, reviews=%d, draft=%t, result=%+v", repairs, reviewerRuns, draft, result)
				}
				return
			}
			if tc.watchOutages == 1 && (watchReads != 2 || waits != 1) {
				t.Fatalf("watch reads = %d, waits = %d, want 2 reads and 1 wait", watchReads, waits)
			}
			if metadataRetry && (!slices.Equal(labels, []string{"existing label", "tracking", "priority, high"}) ||
				!slices.Equal(assignees, []string{"creator", "maintainer"})) {
				t.Fatalf("retry did not preserve and add metadata: labels %v, assignees %v", labels, assignees)
			}
			wantReady := 1
			if tc.keepDraft {
				wantReady = 0
			}
			if result.Err != nil || result.PRURL != prURL || !result.ReviewRequired || result.ReviewReceipt == nil || !result.PRVerificationPassed ||
				implementations != 1 || creates != 1 || api.posts != wantReviews || result.ReviewReceipt.Head != api.head || verifications != wantVerifications || ready != wantReady || draft != tc.keepDraft {
				t.Fatalf("implementation/create/review/verify/ready = %d/%d/%d/%d/%d, draft=%t, result=%+v",
					implementations, creates, api.posts, verifications, ready, draft, result)
			}
			if delivery != nil {
				if tc.repair && (repairs != 1 || reviewerRuns != 2 || result.ReviewReceipt.Pass != 2) {
					t.Fatalf("repair continuation lost its fix budget: repairs=%d, reviews=%d, receipt=%+v", repairs, reviewerRuns, result.ReviewReceipt)
				}
				if tc.repairHeadLag == 1 && waits != 1 {
					t.Fatalf("expected one in-place head propagation wait, got %d", waits)
				}
				outcome := workflow.Outcome{Status: "succeeded", PRURL: result.PRURL,
					ReviewReceipt: result.ReviewReceipt, ReviewRequired: result.ReviewRequired, PendingReviewGate: result.PendingReviewGate}
				if err := delivery.Complete(activation.ID, outcome); err != nil {
					t.Fatal(err)
				}
				if err := delivery.Tick(t.Context()); err != nil || merges != 0 {
					t.Fatalf("merge ran before GitHub reported passing checks: merges=%d, err=%v", merges, err)
				}
				wantMerges := 1
				if tc.operatorMerge {
					merged, wantMerges = true, 0
				} else {
					mergeChecksReady = true
				}
				for range 2 {
					if err := delivery.Tick(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if dispatches != 1 || merges != wantMerges || mergeReceipts != 1 {
					runs, err := delivery.Runs()
					t.Fatalf("dispatches=%d, merges=%d, receipts=%d, runs=%+v, err=%v", dispatches, merges, mergeReceipts, runs, err)
				}
			}
			if tc.readyDuringVerifier && (draftRestorations != 1 || reviewerRuns != 1) {
				t.Fatalf("late readiness triggered %d draft restorations and %d reviewer runs", draftRestorations, reviewerRuns)
			}
			if tc.retry && reviewerRuns != 1 {
				t.Fatalf("same-head verification retry invoked the reviewer %d times", reviewerRuns)
			}
			if tc.earlyReady && verificationPolicyReads != 3 {
				t.Fatalf("policy queried %d times, want initial read and two passing-rollup refreshes", verificationPolicyReads)
			}
			if tc.largePatch {
				if policy := result.ReviewReceipt.Policy; policy.FixPasses != 10 || policy.Complexity != "easy" {
					t.Fatalf("large-patch delivery used the wrong review policy: %+v", policy)
				}
				if _, err := os.Stat(artifactPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("temporary reviewed patch still exists: %v", err)
				}
			}
		})
	}
}

func newIssueDeliveryTestEngine(t *testing.T, root string, runner execx.Runner, passes int, submit func(workflow.Run)) *workflow.Engine {
	t.Helper()
	policy := workflow.ExecutionPolicy{ReviewCycles: passes, ReviewAgent: config.ReviewAgentConfig{Complexity: "easy"}}
	definition := workflow.Definition{ID: "delivery", Name: "Delivery", Repository: "acme/repo", Enabled: true, IncludeBacklog: true,
		Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all"},
		Task:  workflow.Task{Kind: "implement", Instructions: "Update work.txt.", IssueLinkMode: "reference"},
		Merge: workflow.Merge{Enabled: true, Method: "squash"}}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, DefaultExecution: &policy, Workflows: []workflow.Definition{definition}}); err != nil {
		t.Fatal(err)
	}
	gh := workflow.GitHub{Runner: runner}
	return &workflow.Engine{Store: store, StatePath: filepath.Join(root, "workflow-state.json"),
		Enabled: func() bool { return true }, Allowed: func(string) bool { return true },
		Fetch: gh.Fetch, InspectPR: gh.Inspect, InspectPRState: gh.InspectState, Merge: gh.Merge, FinishReview: New(runner).ResumeReviewGate,
		Submit: func(_ context.Context, run workflow.Run) (string, error) {
			submit(run)
			return "issue-task", nil
		},
		Status: func(string) workflow.Outcome { return workflow.Outcome{Status: "running"} },
	}
}
