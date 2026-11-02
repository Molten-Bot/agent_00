package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

// agentPromptIsRemediation reports whether an agent invocation carried the CI
// remediation prompt for the given attempt. Every agent harness passes the
// prompt in execx.Command.Stdin (never argv), and commandsEqual compares only
// Name/Dir/Args, so command equality alone cannot tell a remediation
// invocation apart from the initial task invocation: both run the same binary,
// in the same directory, with the same arguments.
func agentPromptIsRemediation(cmd execx.Command, attempt int) bool {
	marker := fmt.Sprintf("Remediation round %d/%d.", attempt, maxPRCheckRemediationAttempts)
	return strings.Contains(cmd.Stdin, marker)
}

// Guards the trap above: a remediation invocation must stay distinguishable
// from the initial task invocation even though the two commands are equal.
func TestAgentPromptIsRemediationDistinguishesInvocationsCommandEqualityCannot(t *testing.T) {
	t.Parallel()

	basePrompt := "Build API"
	initial := execx.Command{Name: "codex", Dir: "/repo", Args: []string{"exec"}, Stdin: basePrompt}
	repair := initial
	repair.Stdin = remediationPrompt(basePrompt, "https://github.com/acme/repo/pull/42", "X unit-tests failing", 1)

	if !commandsEqual(initial, repair) {
		t.Fatal("commandsEqual(initial, remediation) = false, want true (the prompt lives in Stdin, not Args)")
	}
	if agentPromptIsRemediation(initial, 1) {
		t.Fatal("agentPromptIsRemediation(initial invocation) = true, want false")
	}
	if !agentPromptIsRemediation(repair, 1) {
		t.Fatal("agentPromptIsRemediation(remediation attempt 1) = false, want true")
	}
	if agentPromptIsRemediation(repair, 2) {
		t.Fatal("agentPromptIsRemediation(remediation attempt 1, asked for attempt 2) = true, want false")
	}
}

// `git status --porcelain --branch` always prints a "## <branch>" header, so a
// remediation round that changed nothing still produces non-empty output. The
// run must recognize that as "no remediation changes" and stop on the first
// attempt instead of committing nothing, pushing nothing, and spending every
// remaining attempt on an agent with nothing to change.
func TestRunRemediationBranchHeaderOnlyStatusStopsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "abcdef123456"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	prURL := "https://github.com/acme/repo/pull/42"
	checkSummary := "X unit-tests failing"
	remediationPromptText := remediationPrompt(withAgentsPrompt(cfg.Prompt, agentsPath), prURL, checkSummary, 1)

	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"auth", "status"}}},
		{cmd: cloneCommand(cfg, repoDir)},
		{cmd: branchCommand(repoDir, branch)},
		{cmd: pushDryRunCommand(repoDir, branch)},
		{cmd: codexCommand(targetDir, withAgentsPrompt(cfg.Prompt, agentsPath))},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: " M file.go\n"}},
		{cmd: addCommand(repoDir)},
		{cmd: commitCommand(repoDir, cfg.CommitMessage)},
		{cmd: pushCommand(repoDir, branch)},
		{cmd: prCreateCommand(repoDir, cfg, branch), res: execx.Result{Stdout: prURL + "\n"}},
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: checkSummary + "\n"}, err: errors.New("checks failed")},
		{cmd: codexCommand(targetDir, remediationPromptText)},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: "## " + branch + "...origin/" + branch + "\n"}},
	}}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	res := h.Run(context.Background(), cfg)
	if res.Err == nil {
		t.Fatal("Run() err = nil, want no-remediation-changes failure")
	}
	if res.ExitCode != ExitPR {
		t.Fatalf("ExitCode = %d, want %d", res.ExitCode, ExitPR)
	}
	if !strings.Contains(res.Err.Error(), "no remediation changes") {
		t.Fatalf("Run() err = %v, want no-remediation-changes detail", res.Err)
	}
	if res.PRURL != prURL {
		t.Fatalf("PRURL = %q, want %q", res.PRURL, prURL)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}

	remediationCalls := 0
	for _, call := range fake.calls {
		if agentPromptIsRemediation(call, 1) {
			remediationCalls++
		}
	}
	if remediationCalls != 1 {
		t.Fatalf("remediation agent invocations = %d, want 1", remediationCalls)
	}

	joinedLogs := strings.Join(logs, "\n")
	if strings.Contains(joinedLogs, "action=repair_commit") {
		t.Fatalf("logs show a repair commit for a no-op remediation round:\n%s", joinedLogs)
	}
}

// A remediation round that only committed locally (clean worktree, branch ahead
// of its upstream) is real progress and must still be pushed.
func TestRunRemediationLocalCommitOnlyStillPushes(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "abcdef123456"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	prURL := "https://github.com/acme/repo/pull/42"
	checkSummary := "X unit-tests failing"
	aheadStatus := "## " + branch + "...origin/" + branch + " [ahead 1]\n"

	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"auth", "status"}}},
		{cmd: cloneCommand(cfg, repoDir)},
		{cmd: branchCommand(repoDir, branch)},
		{cmd: pushDryRunCommand(repoDir, branch)},
		{cmd: codexCommand(targetDir, withAgentsPrompt(cfg.Prompt, agentsPath))},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: " M file.go\n"}},
		{cmd: addCommand(repoDir)},
		{cmd: commitCommand(repoDir, cfg.CommitMessage)},
		{cmd: pushCommand(repoDir, branch)},
		{cmd: prCreateCommand(repoDir, cfg, branch), res: execx.Result{Stdout: prURL + "\n"}},
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: checkSummary + "\n"}, err: errors.New("checks failed")},
		{cmd: codexCommand(targetDir, remediationPrompt(withAgentsPrompt(cfg.Prompt, agentsPath), prURL, checkSummary, 1))},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: aheadStatus}},
		{cmd: addCommand(repoDir)},
		{
			cmd: commitCommand(repoDir, remediationCommitMessage(cfg.CommitMessage, 1)),
			res: execx.Result{Stdout: "On branch " + branch + "\nnothing to commit, working tree clean\n"},
			err: errors.New("exit status 1"),
		},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: aheadStatus}},
		{cmd: commitsAheadOfBaseCommand(repoDir, cfg.BaseBranch), res: execx.Result{Stdout: "1\n"}},
		{cmd: pushCommand(repoDir, branch)},
		{cmd: prChecksCommand(repoDir, prURL)},
	}}

	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }

	res := h.Run(context.Background(), cfg)
	if res.Err != nil {
		t.Fatalf("Run() err = %v", res.Err)
	}
	if res.ExitCode != ExitSuccess {
		t.Fatalf("ExitCode = %d, want %d", res.ExitCode, ExitSuccess)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}
}

// gh pads its "Refreshing checks status" banner differently across versions;
// a tab-padded banner must never be parsed as a check row.
func TestLatestCheckSnapshotFromTextSkipsTabPaddedRefreshBanner(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		"Refreshing checks status every 10 seconds.\tPress Ctrl+C to quit.",
		"check-title\tfail\t6s\thttps://github.com/acme/repo/actions/runs/1/job/11",
	}, "\n")

	snapshot, err := latestCheckSnapshotFromText(raw)
	if err != nil {
		t.Fatalf("latestCheckSnapshotFromText() err = %v", err)
	}
	if got, want := snapshot.Summary, "check-title\tfail"; got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	if got, want := strings.Join(snapshot.FailingNames, ","), "check-title"; got != want {
		t.Fatalf("FailingNames = %q, want %q", got, want)
	}
}
