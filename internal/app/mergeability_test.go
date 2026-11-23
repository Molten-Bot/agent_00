package app

import (
	"context"
	"errors"
	"fmt"
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
			code, _, err := h.ensurePullRequestMergeable(context.Background(), config.Config{}, repo, agentruntime.Default(), repo.Dir, codexRunOptions{}, "", "", "repo", "codex")
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
					return execx.Result{Stdout: fmt.Sprintf(`{"state":"OPEN","mergeable":%q,"baseRefName":"main","headRefName":"feature"}`, state)}, nil
				}
				if cmd.Name == "git" && len(cmd.Args) > 0 {
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
		})
	}
}
