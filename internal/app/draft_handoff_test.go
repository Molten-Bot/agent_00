package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestClaimedIssueDraftHandoff(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-gh-token")
	t.Setenv("GITHUB_TOKEN", "test-github-token")
	const (
		branch       = "agent00-issue-17"
		repoURL      = "https://github.com/acme/repo.git"
		prURL        = "https://github.com/acme/repo/pull/42"
		requirement  = "Verify the complete live changed-file inventory, the change to work.txt, the submitted clean review, and the CI result for the draft head."
		localSuccess = "Implementation and local checks passed.\nDraft handoff: " + requirement
	)
	for _, tc := range []struct {
		name, answer        string
		change              bool
		agentErr            error
		retry               bool
		metadataError       string
		findings            bool
		headMoved           bool
		pendingChecks       int
		checksFail          bool
		filesPartial        bool
		filesTruncated      bool
		patchTruncated      bool
		reviewStale         bool
		reviewTruncated     bool
		queued              bool
		wantExit            int
		wantTruncationError string
	}{
		{name: "verify private draft from supplied evidence", answer: localSuccess, change: true},
		{name: "wait for pending checks on reviewed head", answer: localSuccess, change: true, pendingChecks: 1},
		{name: "pending checks outlive the wait budget", answer: localSuccess, change: true, pendingChecks: 3, wantExit: ExitPR},
		{name: "head moves while checks are pending", answer: localSuccess, change: true, pendingChecks: 1, headMoved: true, wantExit: ExitPR},
		{name: "failed checks block verification", answer: localSuccess, change: true, checksFail: true, wantExit: ExitPR},
		{name: "retry verification on the same draft", answer: localSuccess, change: true, retry: true, wantExit: ExitPR},
		{name: "retry rejected assignee on the same draft", answer: localSuccess, change: true, metadataError: "assignee", wantExit: ExitPR},
		{name: "retry rejected label on the same draft", answer: localSuccess, change: true, metadataError: "label", wantExit: ExitPR},
		{name: "findings keep the draft blocked", answer: localSuccess, change: true, findings: true, wantExit: ExitPR},
		{name: "head changed before verification", answer: localSuccess, change: true, headMoved: true, wantExit: ExitPR},
		{name: "changed-file evidence unavailable", answer: localSuccess, change: true, filesPartial: true, wantExit: ExitPR},
		{name: "changed-file inventory exceeds evidence limit", answer: localSuccess, change: true, filesTruncated: true, wantExit: ExitPR,
			wantTruncationError: "live changed-file inventory exceeds the draft verification evidence limit"},
		{name: "reviewed patch exceeds evidence limit", answer: localSuccess, change: true, patchTruncated: true, wantExit: ExitPR,
			wantTruncationError: "reviewed patch exceeds the draft verification evidence limit"},
		{name: "submitted review stale", answer: localSuccess, change: true, reviewStale: true, wantExit: ExitPR},
		{name: "submitted review body exceeds evidence limit", answer: localSuccess, change: true, reviewTruncated: true, wantExit: ExitPR,
			wantTruncationError: "submitted review 1 body exceeds the draft verification evidence limit"},
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
			var implementations, creates, verifications, ready, evidenceReads, waits int
			var draft, accepted, metadataRetry bool
			savedResume := ""
			labels := []string{"existing label"}
			assignees := []string{"creator"}
			var savedRequirement string
			hasGitHubCredential := func(cmd execx.Command) bool {
				if cmd.Env == nil {
					return os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != ""
				}
				return slices.ContainsFunc(cmd.Env, func(entry string) bool {
					return strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "GITHUB_TOKEN=")
				})
			}
			runner := issueBranchRunner(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				args := strings.Join(cmd.Args, " ")
				if cmd.Name == "gh" && strings.Contains(args, "--method POST repos/acme/repo/issues/42/") && savedResume != prURL {
					t.Fatal("metadata mutation preceded saved retry target")
				}
				if res, ok, err := api.handle(cmd); ok {
					return res, err
				}
				switch cmd.Name {
				case "git":
					if args == "remote get-url origin" {
						return execx.Result{Stdout: repoURL}, nil
					}
					if i := slices.Index(cmd.Args, repoURL); i >= 0 {
						cmd.Args = slices.Clone(cmd.Args)
						cmd.Args[i] = remote
					}
					cmd.Args = append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, cmd.Args...)
					return (execx.OSRunner{}).Run(ctx, cmd)
				case "codex":
					switch {
					case args == "--help":
						return execx.Result{}, nil
					case strings.Contains(cmd.Stdin, "Verify the remaining acceptance criteria against live GitHub evidence"):
						verifications++
						if hasGitHubCredential(cmd) || !strings.Contains(args, "network={enabled=false}") {
							t.Error("private draft verifier must run without GitHub credentials or network access")
						}
						if !draft || api.posts != 1 || !strings.Contains(cmd.Stdin, requirement) ||
							!strings.Contains(cmd.Stdin, `"state":"OPEN","isDraft":true`) ||
							!strings.Contains(cmd.Stdin, `"headRefOid":"`+api.head+`"`) ||
							!strings.Contains(cmd.Stdin, `"commit_id":"`+api.head+`"`) ||
							!strings.Contains(cmd.Stdin, "#pullrequestreview-1") || !strings.Contains(cmd.Stdin, "No material issues found.") ||
							!strings.Contains(cmd.Stdin, `"conclusion":"SUCCESS"`) || !strings.Contains(cmd.Stdin, `"path":"work.txt"`) ||
							!strings.Contains(cmd.Stdin, "+implemented") {
							t.Error("verification started before draft and submitted evidence were available")
						}
						if tc.filesPartial {
							if !strings.Contains(cmd.Stdin, "Incomplete: the returned file count") {
								t.Error("partial live file inventory was not marked incomplete")
							}
							return execx.Result{Stdout: `{"status":"blocked","evidence":"Need the complete live changed-file inventory from GitHub."}`}, nil
						}
						if tc.retry && verifications == 1 {
							return execx.Result{Stdout: `{"status":"blocked","evidence":"Missing live evidence"}`}, nil
						}
						return execx.Result{Stdout: `{"status":"passed","evidence":"The live inventory lists work.txt, its patch shows the change, and CI and the clean review match the draft head."}`}, nil
					case strings.Contains(cmd.Stdin, "Prepared pull-request review context"):
						if !draft {
							t.Error("review ran before draft publication")
						}
						if tc.findings {
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
							if tc.patchTruncated {
								content += strings.Repeat("extra line\n", maxReviewDiffPatchChars/len("extra line\n")+1)
							}
							if err := os.WriteFile(filepath.Join(cmd.Dir, "work.txt"), []byte(content), 0o600); err != nil {
								return execx.Result{}, err
							}
						}
						return execx.Result{Stdout: tc.answer}, tc.agentErr
					}
				case "gh":
					switch {
					case args == "api repos/acme/repo/pulls/42/reviews/1":
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
						if !slices.Contains(cmd.Args, "--draft") || !accepted || savedRequirement != requirement {
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
						}
						draft = slices.Contains(cmd.Args, "--undo")
						return execx.Result{}, nil
					case strings.HasPrefix(args, "pr checks "):
						return execx.Result{Stdout: "CI\tpass\n"}, nil
					case strings.HasPrefix(args, "pr view "):
						head := api.head
						fileEvidence := ""
						check := `"conclusion":"SUCCESS"`
						if strings.Contains(args, "statusCheckRollup") && strings.Contains(args, "changedFiles,files") {
							if !hasGitHubCredential(cmd) {
								return execx.Result{}, errors.New("private draft file metadata requires authentication")
							}
							evidenceReads++
							switch {
							case tc.pendingChecks >= evidenceReads:
								check = `"status":"IN_PROGRESS"`
							case tc.checksFail:
								check = `"conclusion":"FAILURE"`
							}
							if tc.headMoved && evidenceReads > tc.pendingChecks {
								head = "new-head"
							}
							files := []string{`{"path":"work.txt","additions":1,"deletions":1}`}
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
						return execx.Result{Stdout: fmt.Sprintf(`{"number":42,"url":%q,"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main","headRefName":%q,"headRefOid":%q,"isDraft":%t,"statusCheckRollup":[{"name":"CI",%s}]%s}`, prURL, branch, head, draft, check, fileEvidence)}, nil
					case strings.Contains(args, "api repos/acme/repo/pulls?") && creates > 0:
						return execx.Result{Stdout: fmt.Sprintf(`[{"html_url":%q,"title":"Issue","state":"open","head":{"ref":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main"}}]`, prURL, branch)}, nil
					case strings.HasPrefix(args, "api "):
						return execx.Result{Stdout: "[]"}, nil
					}
				}
				return execx.Result{}, fmt.Errorf("unexpected command %s %s", cmd.Name, args)
			})
			h := New(runner)
			h.PRChecksWatchTimeout = 10 * time.Second
			h.Sleep = func(context.Context, time.Duration) error { waits++; return nil }
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
			cfg.RepoURL, cfg.TargetSubdir = repoURL, ""
			cfg.GitHubIssue = &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, URL: "https://github.com/acme/repo/issues/17",
				Claim: &config.GitHubIssueClaim{ID: "claim", Branch: branch, BaseBranch: "main", ExclusionLabel: "claimed", Attempt: 1}}
			if tc.metadataError != "" {
				cfg.Workflow = &config.WorkflowRunConfig{ID: "route", EventID: "route-event", PRAssignee: "maintainer", PRLabels: []string{"tracking", "priority, high"}}
			}
			result := h.Run(t.Context(), cfg)
			if result.ExitCode != tc.wantExit {
				t.Fatalf("issue run exit = %d, want %d: %v", result.ExitCode, tc.wantExit, result.Err)
			}
			if !tc.change || tc.wantExit == ExitCodex {
				if creates != 0 || accepted || git(remote, "rev-parse", branch) != base || result.NoChanges != !tc.change {
					t.Fatalf("unaccepted work was published or no-change result lost: %+v", result)
				}
				return
			}
			if tc.wantExit != ExitSuccess && (!draft || ready != 0 || result.ReviewReceipt != nil || result.PRVerificationPassed) {
				t.Fatalf("failed verification or review released the draft: %+v", result)
			}
			if tc.wantTruncationError != "" {
				if creates != 1 || api.posts != 1 || verifications != 0 || !strings.Contains(fmt.Sprint(result.Err), tc.wantTruncationError) {
					t.Fatalf("expected truncation to block verification with an actionable reason: %+v", result)
				}
				return
			}
			if tc.filesPartial {
				if creates != 1 || api.posts != 1 || verifications != 1 || !strings.Contains(fmt.Sprint(result.Err), "Need the complete live changed-file inventory from GitHub.") {
					t.Fatalf("expected one blocked verification with an actionable reason: %+v", result)
				}
				return
			}
			if tc.pendingChecks > 0 && waits != 1 {
				t.Fatalf("waits = %d, want one pending check retry", waits)
			}
			if tc.pendingChecks > 1 && !errors.Is(result.Err, ErrReviewPending) {
				t.Fatalf("expected pending verification after retry budget, got %v", result.Err)
			}
			if tc.checksFail && !strings.Contains(fmt.Sprint(result.Err), "checks failed on the reviewed pull request head") {
				t.Fatalf("expected failed checks to block verification, got %v", result.Err)
			}
			if tc.findings || tc.headMoved || tc.reviewStale || tc.checksFail || tc.pendingChecks > 1 {
				if creates != 1 || api.posts != 1 || verifications != 0 {
					t.Fatal("expected one submitted review and verification blocked")
				}
				if tc.reviewStale && !strings.Contains(fmt.Sprint(result.Err), "submitted review 1 to show a clean verdict on the draft head") {
					t.Fatalf("expected the stale review to block verification: %+v", result)
				}
				return
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
			if tc.retry || metadataRetry {
				h.ResumeIssueWorkspace = true
				h.IssueCheckoutComplete = true
				h.IssuePublicationReady = accepted
				h.IssuePRVerification = savedRequirement
				cfg.GitHubIssue.Claim.Attempt++
				result = h.Run(t.Context(), cfg)
				if tc.retry {
					wantVerifications++
				}
			}
			if metadataRetry && (!slices.Equal(labels, []string{"existing label", "tracking", "priority, high"}) ||
				!slices.Equal(assignees, []string{"creator", "maintainer"})) {
				t.Fatalf("retry did not preserve and add metadata: labels %v, assignees %v", labels, assignees)
			}
			if result.Err != nil || result.PRURL != prURL || !result.ReviewRequired || result.ReviewReceipt == nil || !result.PRVerificationPassed ||
				implementations != 1 || creates != 1 || api.posts != 1 || verifications != wantVerifications || ready != 1 || draft {
				t.Fatalf("implementation/create/review/verify/ready = %d/%d/%d/%d/%d, draft=%t, result=%+v",
					implementations, creates, api.posts, verifications, ready, draft, result)
			}
		})
	}
}
