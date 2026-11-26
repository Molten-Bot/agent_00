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

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestClaimedIssueDraftHandoff(t *testing.T) {
	t.Parallel()
	const (
		branch       = "agent00-issue-17"
		repoURL      = "https://github.com/acme/repo.git"
		prURL        = "https://github.com/acme/repo/pull/42"
		requirement  = "Verify the submitted clean review and the CI result for the draft head."
		localSuccess = "Implementation and local checks passed.\nDraft handoff: " + requirement
	)
	for _, tc := range []struct {
		name, answer string
		change       bool
		agentErr     error
		retry        bool
		findings     bool
		headMoved    bool
		queued       bool
		wantExit     int
	}{
		{name: "publish and verify", answer: localSuccess, change: true},
		{name: "retry verification on the same draft", answer: localSuccess, change: true, retry: true, wantExit: ExitPR},
		{name: "findings keep the draft blocked", answer: localSuccess, change: true, findings: true, wantExit: ExitPR},
		{name: "head changed before verification", answer: localSuccess, change: true, headMoved: true, wantExit: ExitPR},
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
			var implementations, creates, verifications, ready int
			var draft, accepted bool
			var savedRequirement string
			runner := issueBranchRunner(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				args := strings.Join(cmd.Args, " ")
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
						if !draft || api.posts != 1 || !strings.Contains(cmd.Stdin, requirement) ||
							!strings.Contains(cmd.Stdin, "#pullrequestreview-1") || !strings.Contains(cmd.Stdin, `"conclusion":"SUCCESS"`) {
							t.Error("verification started before draft and submitted evidence were available")
						}
						if tc.retry && verifications == 1 {
							return execx.Result{Stdout: `{"status":"blocked","evidence":"Missing live evidence"}`}, nil
						}
						return execx.Result{Stdout: `{"status":"passed","evidence":"CI and the submitted clean review match the draft head"}`}, nil
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
							if err := os.WriteFile(filepath.Join(cmd.Dir, "work.txt"), []byte("implemented\n"), 0o600); err != nil {
								return execx.Result{}, err
							}
						}
						return execx.Result{Stdout: tc.answer}, tc.agentErr
					}
				case "gh":
					switch {
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
						if tc.headMoved && strings.Contains(args, "statusCheckRollup") {
							head = "new-head"
						}
						return execx.Result{Stdout: fmt.Sprintf(`{"number":42,"url":%q,"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main","headRefName":%q,"headRefOid":%q,"isDraft":%t,"statusCheckRollup":[{"name":"CI","conclusion":"SUCCESS"}]}`, prURL, branch, head, draft)}, nil
					case strings.Contains(args, "api repos/acme/repo/pulls?") && creates > 0:
						return execx.Result{Stdout: fmt.Sprintf(`[{"html_url":%q,"title":"Issue","state":"open","head":{"ref":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main"}}]`, prURL, branch)}, nil
					case strings.HasPrefix(args, "api "):
						return execx.Result{Stdout: "[]"}, nil
					}
				}
				return execx.Result{}, fmt.Errorf("unexpected command %s %s", cmd.Name, args)
			})
			h := New(runner)
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
			if tc.findings || tc.headMoved {
				if creates != 1 || api.posts != 1 || verifications != 0 {
					t.Fatal("expected one submitted review and verification blocked")
				}
				return
			}
			wantVerifications := 1
			if tc.retry {
				h.ResumeIssueWorkspace = true
				h.IssueCheckoutComplete = true
				h.IssuePublicationReady = accepted
				h.IssuePRVerification = savedRequirement
				cfg.GitHubIssue.Claim.Attempt++
				result = h.Run(t.Context(), cfg)
				wantVerifications++
			}
			if result.Err != nil || result.PRURL != prURL || !result.ReviewRequired || result.ReviewReceipt == nil || !result.PRVerificationPassed ||
				implementations != 1 || creates != 1 || api.posts != 1 || verifications != wantVerifications || ready != 1 || draft {
				t.Fatalf("implementation/create/review/verify/ready = %d/%d/%d/%d/%d, draft=%t, result=%+v",
					implementations, creates, api.posts, verifications, ready, draft, result)
			}
		})
	}
}
