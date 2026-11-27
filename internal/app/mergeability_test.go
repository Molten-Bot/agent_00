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
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestMergeabilityVerification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, state string
		wantErr     bool
		checks      int
	}{
		{"clean", `{"state":"OPEN","mergeable":"MERGEABLE"}`, false, 1},
		{"unknown", `{"state":"OPEN","mergeable":"UNKNOWN"}`, true, 6},
		{"missing", `{"state":"OPEN"}`, true, 1},
		{"malformed", `{`, true, 1},
		{"closed", `{"state":"CLOSED"}`, true, 1},
		{"merged", `{"state":"MERGED"}`, false, 1},
		{"wrong head", `{"state":"OPEN","mergeable":"CONFLICTING","baseRefName":"main","headRefName":"other"}`, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &repoWorkspace{Dir: t.TempDir(), Branch: "feature", PRURL: "https://github.com/acme/repo/pull/7"}
			checks := 0
			h := New(sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				if !commandsEqual(cmd, prMergeabilityCommand(repo.Dir, repo.PRURL)) {
					t.Fatalf("unexpected command: %+v", cmd)
				}
				checks++
				return execx.Result{Stdout: tc.state}, nil
			}))
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			code, _, err := h.ensurePullRequestMergeable(context.Background(), config.Config{}, repo, agentruntime.Default(), repo.Dir, codexRunOptions{}, "", "", "repo", "codex", "", false)
			if (err != nil) != tc.wantErr || (code != ExitSuccess) != tc.wantErr || checks != tc.checks {
				t.Fatalf("code=%d err=%v checks=%d", code, err, checks)
			}
		})
	}
}

func TestMergeabilityRepairsExistingBranchBeforeReviewAndAtCompletion(t *testing.T) {
	for _, phase := range []string{"before review", "after review", "reviews disabled", "repair fails", "verification fails"} {
		t.Run(phase, func(t *testing.T) {
			repo := &repoWorkspace{URL: "https://github.com/acme/repo.git", Dir: t.TempDir(), RelDir: "repo", Branch: "feature", BaseBranch: "feature", PRURL: "https://github.com/acme/repo/pull/7", Changed: true, RequireExistingPR: true, WriteAccessChecked: true, WriteAccessAllowed: true}
			clean := &cleanReviewCycleRunner{t: t}
			checks, repairs, merges, pushes, repairCommits := 0, 0, 0, 0, 0
			conflict := false
			admitted := true
			h := New(sessionRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				if commandsEqual(cmd, prMergeabilityCommand(repo.Dir, repo.PRURL)) {
					checks++
					shouldConflict := repairs == 0 && (phase != "after review" || clean.reviews > 0)
					if phase == "verification fails" {
						shouldConflict = true
					}
					state := "MERGEABLE"
					if shouldConflict {
						state = "CONFLICTING"
						conflict = true
					}
					return execx.Result{Stdout: fmt.Sprintf(`{"state":"OPEN","mergeable":%q,"baseRefName":"main","headRefName":"feature","headRefOid":"head-sha"}`, state)}, nil
				}
				if cmd.Name == "git" && len(cmd.Args) > 0 {
					if !admitted {
						t.Fatalf("ran local git operation without admission: %v", cmd.Args)
					}
					switch cmd.Args[0] {
					case "status":
						if repairs > 0 && repairCommits == 0 {
							return execx.Result{Stdout: "## feature\nM  file.go\n"}, nil
						}
					case "commit":
						if repairs > 0 {
							repairCommits++
						}
					case "ls-remote":
						return execx.Result{Stdout: "head-sha\trefs/heads/feature\n"}, nil
					case "fetch":
						if slices.Contains(cmd.Args, "feature") {
							t.Fatalf("must fetch actual PR base: %+v", cmd)
						}
					case "merge":
						merges++
						if conflict && repairs == 0 {
							return execx.Result{Stdout: "CONFLICT (content): Merge conflict in file.go"}, errors.New("merge conflict")
						}
					case "push":
						pushes++
					}
				}
				if cmd.Name == "codex" && strings.Contains(cmd.Stdin, "Base-branch sync conflict remediation") {
					if phase == "before review" && clean.reviews != 0 {
						t.Fatal("review ran before conflict repair")
					}
					if phase == "after review" && clean.reviews == 0 {
						t.Fatal("expected final repair after review")
					}
					repairs++
					if phase == "repair fails" {
						return execx.Result{}, errors.New("repair failed")
					}
					return execx.Result{Stdout: conflictResolutionConfirmation}, nil
				}
				if cmd.Name == "git" && slices.Contains(cmd.Args, "--check") {
					return execx.Result{}, nil
				}
				return clean.Run(ctx, cmd)
			}))
			h.ReleaseWorkspace = func() { admitted = false }
			h.AcquireWorkspace = func(context.Context) error { admitted = true; return nil }
			h.FinalReviewPasses = 1
			if phase == "reviews disabled" {
				h.FinalReviewPasses = 0
			}
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			code, _, err := h.processChangedRepo(context.Background(), config.Config{Prompt: "Fix behavior", CommitMessage: "fix behavior"}, repo, agentruntime.Default(), repo.Dir, codexRunOptions{}, "", "", "repo", "codex", false)
			wantErr := phase == "repair fails" || phase == "verification fails"
			if (err != nil) != wantErr || (code != ExitSuccess) != wantErr {
				t.Fatalf("code=%d err=%v", code, err)
			}
			if repairs != 1 || merges == 0 {
				t.Fatalf("repairs=%d merges=%d", repairs, merges)
			}
			if wantErr {
				if clean.released != 0 {
					t.Fatal("released conflicting PR")
				}
				return
			}
			if pushes < 2 || checks < 2 || repairCommits != 1 {
				t.Fatalf("pushes=%d checks=%d repairCommits=%d", pushes, checks, repairCommits)
			}
			if phase == "reviews disabled" && clean.reviews != 0 {
				t.Fatal("unexpected review")
			}
			if phase == "after review" && (clean.checks != 2 || clean.reviews != 2 || clean.released != 1) {
				t.Fatalf("late repair: checks=%d reviews=%d ready writes=%d", clean.checks, clean.reviews, clean.released)
			}
		})
	}
}

// Exercise a real merge and push while GitHub check responses stay pending on
// the original head.
func TestPublishedChecksRepairConflictWhilePending(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	remote, checkout, baseCheckout := t.TempDir(), t.TempDir(), t.TempDir()
	gitRunner := execx.OSRunner{}
	git := func(dir string, args ...string) string {
		t.Helper()
		res, err := gitRunner.Run(ctx, gitCommand(dir, args...))
		if err != nil {
			t.Fatalf("git %v: %v\n%s\n%s", args, err, res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}
	writeFixture := func(dir, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "fixture.txt"), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(remote, "init", "--bare", "--initial-branch=main")
	git(checkout, "init", "--initial-branch=main")
	git(checkout, "config", "user.name", "Test")
	git(checkout, "config", "user.email", "test@example.com")
	git(checkout, "config", "commit.gpgsign", "false")
	git(checkout, "remote", "add", "origin", remote)
	writeFixture(checkout, "original")
	git(checkout, "add", "fixture.txt")
	git(checkout, "commit", "-m", "Initial fixture")
	git(checkout, "push", "origin", "main")
	git(baseCheckout, "clone", remote, ".")
	git(baseCheckout, "config", "user.name", "Test")
	git(baseCheckout, "config", "user.email", "test@example.com")
	git(baseCheckout, "config", "commit.gpgsign", "false")
	git(checkout, "switch", "-c", "feature")
	writeFixture(checkout, "feature edit")
	git(checkout, "commit", "-am", "Feature edit")
	git(checkout, "push", "origin", "feature")
	original := git(remote, "rev-parse", "feature")

	const url = "https://github.com/example/repo/pull/42"
	repo := repoWorkspace{Dir: checkout, RelDir: "repo", Branch: "feature", BaseBranch: "main", PRURL: url, RequireExistingPR: true}
	admitted := true
	var checks, repairs, sleeps int
	var logs []string
	h := New(sessionRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		switch {
		case commandsEqual(cmd, prMergeabilityCommand(checkout, url)):
			published := git(remote, "rev-parse", "feature")
			mergeable := "MERGEABLE"
			if res, err := gitRunner.Run(ctx, gitCommand(remote, "merge-tree", "--write-tree", "main", published)); err != nil {
				if !strings.Contains(res.Stdout, "CONFLICT") {
					t.Fatalf("compute mergeability: %v\n%s\n%s", err, res.Stdout, res.Stderr)
				}
				mergeable = "CONFLICTING"
			}
			return execx.Result{Stdout: fmt.Sprintf(`{"state":"OPEN","mergeable":%q,"baseRefName":"main","headRefName":"feature","headRefOid":%q}`, mergeable, published)}, nil
		case commandsEqual(cmd, prChecksCommand(checkout, url)):
			checks++
			published := git(remote, "rev-parse", "feature")
			if checks == 1 && published == original {
				return execx.Result{Stdout: "CI\tpending\n"}, pendingCheckExit{}
			}
			if checks != 2 || published == original || repairs != 1 {
				t.Fatalf("checked unrepaired head or restarted unexpectedly: head=%s checks=%d repairs=%d", published, checks, repairs)
			}
			git(remote, "merge-base", "--is-ancestor", "main", published)
			git(remote, "merge-base", "--is-ancestor", original, published)
			if got := git(remote, "show", published+":fixture.txt"); got != "base edit and feature edit" {
				t.Fatalf("published fixture = %q", got)
			}
			return execx.Result{Stdout: "CI\tpass\n"}, nil
		case cmd.Name == "git" && admitted:
			return gitRunner.Run(ctx, cmd)
		case cmd.Name == "codex" && admitted && strings.Contains(cmd.Stdin, "Base-branch sync conflict remediation"):
			repairs++
			if checks != 1 || sleeps != 1 {
				t.Fatalf("repair did not replace pending checks: checks=%d sleeps=%d", checks, sleeps)
			}
			writeFixture(checkout, "base edit and feature edit")
			return execx.Result{Stdout: conflictResolutionConfirmation}, nil
		default:
			t.Fatalf("unexpected command or missing admission: %s %v", cmd.Name, cmd.Args)
			return execx.Result{}, nil
		}
	}))
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	h.AcquireWorkspace = func(context.Context) error { admitted = true; return nil }
	h.ReleaseWorkspace = func() { admitted = false }
	h.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		if admitted || sleeps != 1 {
			t.Fatalf("unexpected wait during repair: admitted=%t sleeps=%d", admitted, sleeps)
		}
		writeFixture(baseCheckout, "base edit")
		git(baseCheckout, "commit", "-am", "Advance base")
		git(baseCheckout, "push", "origin", "main")
		return nil
	}
	cfg, runtime := config.Config{}, agentruntime.Default()
	if code, stage, err := h.checkPublishedPullRequest(ctx, cfg, &repo, runtime, checkout, codexRunOptions{}, "", "", "repo", "codex", false); err != nil || code != ExitSuccess {
		t.Fatalf("checks failed at %s with code %d: %v", stage, code, err)
	}
	if code, stage, err := h.finishPublishedPullRequest(ctx, cfg, &repo, runtime, checkout, codexRunOptions{}, "", "", "repo", "codex", false); err != nil || code != ExitSuccess {
		t.Fatalf("final mergeability gate failed at %s with code %d: %v", stage, code, err)
	}
	if repo.PRURL != url || repo.MergeConflictRepairs != 1 || checks != 2 || repairs != 1 {
		t.Fatalf("PR=%s conflict repairs=%d checks=%d agents=%d", repo.PRURL, repo.MergeConflictRepairs, checks, repairs)
	}
	published := git(remote, "rev-parse", "feature")
	var events []string
	for _, line := range logs {
		switch {
		case strings.Contains(line, "stage=merge_conflict status=start action=detect") && strings.Contains(line, "head="+original):
			events = append(events, "detect")
		case strings.Contains(line, "stage=codex status=start") && strings.Contains(line, "mode=base_sync_conflict"):
			events = append(events, "repair")
		case strings.Contains(line, "stage=merge_conflict status=ok action=publish_repair") && strings.Contains(line, "head="+published):
			events = append(events, "publish")
		}
	}
	if !slices.Equal(events, []string{"detect", "repair", "publish"}) {
		t.Fatalf("repair events = %v", events)
	}
}
