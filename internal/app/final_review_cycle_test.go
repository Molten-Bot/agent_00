package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// cleanReviewCycleRunner answers every review pass with a clean verdict unless
// reviewStdout replaces that payload. Repair passes are counted separately so a
// test can tell a review invocation from the fix invocation it triggered.
type cleanReviewCycleRunner struct {
	t            *testing.T
	reviewStdout string
	mu           sync.Mutex
	reviews      int
	fixes        int
	fixPrompt    string
	comments     int
	checks       int
	released     int
	prompts      []string
	argv         [][]string
}

func (r *cleanReviewCycleRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	switch cmd.Name {
	case "codex":
		if strings.Contains(cmd.Stdin, "Automated review repair pass") {
			r.fixes++
			r.fixPrompt = cmd.Stdin
			return execx.Result{Stdout: "Reviewed the findings; no change was warranted."}, nil
		}
		r.reviews++
		r.prompts = append(r.prompts, cmd.Stdin)
		r.argv = append(r.argv, append([]string(nil), cmd.Args...))
		if r.reviewStdout != "" {
			return execx.Result{Stdout: r.reviewStdout}, nil
		}
		return execx.Result{Stdout: "**Positive**\n- Scoped change.\n\n**Negative**\n- No material issues found.\n\n```json\n{\"status\":\"clean\",\"mergeReady\":true,\"summary\":\"clean\",\"positives\":[\"Scoped change.\"],\"findings\":[]}\n```"}, nil
	case "git":
		if len(cmd.Args) == 0 {
			break
		}
		switch cmd.Args[0] {
		case "rev-parse":
			return execx.Result{Stdout: "head-sha\n"}, nil
		case "status":
			return execx.Result{Stdout: "## feature\n"}, nil
		case "fetch", "merge", "add", "commit", "push":
			return execx.Result{}, nil
		case "diff":
			return execx.Result{Stdout: "diff --git a/main.go b/main.go\n"}, nil
		}
	case "gh":
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "view" {
			return execx.Result{Stdout: `{"number":7,"title":"Change","url":"https://github.com/acme/repo/pull/7","state":"OPEN","baseRefName":"main","headRefName":"feature","headRefOid":"head-sha","author":{"login":"dev"}}`}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "review" {
			r.comments++
			return execx.Result{}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "checks" {
			r.checks++
			return execx.Result{Stdout: "tests\tpass\n"}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "ready" {
			r.released++
			return execx.Result{}, nil
		}
		if len(cmd.Args) >= 1 && cmd.Args[0] == "api" {
			return execx.Result{Stdout: "[]"}, nil
		}
	}
	r.t.Fatalf("unexpected command: %+v", cmd)
	return execx.Result{}, fmt.Errorf("unexpected command")
}

func TestRunFinalReviewCycleStopsAfterCleanPass(t *testing.T) {
	runner := &cleanReviewCycleRunner{t: t}
	h := New(runner)
	h.FinalReviewPasses = 3

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:        "https://github.com/acme/repo.git",
		Dir:        repoDir,
		RelDir:     "repo",
		Branch:     "feature",
		BaseBranch: "main",
		PRURL:      "https://github.com/acme/repo/pull/7",
		Changed:    true,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior."},
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if runner.reviews != 1 || runner.comments != 1 {
		t.Fatalf("review/comment counts = %d/%d, want 1/1", runner.reviews, runner.comments)
	}
	for i, prompt := range runner.prompts {
		if !strings.Contains(prompt, "The bundled review skill is mandatory") || !strings.Contains(prompt, fmt.Sprintf("Post-task review pass %d/3", i+1)) {
			t.Fatalf("review prompt %d did not include skill and pass context", i+1)
		}
	}
}

func TestBlockingReviewFindingsSplitsAdvisoryFromBlockingSeverities(t *testing.T) {
	t.Parallel()

	findings := []reviewFinding{
		{Severity: "Critical", Title: "Data loss"},
		{Severity: "low", Title: "Rename the local variable"},
		{Severity: " Nit ", Title: "Spacing"},
		{Severity: "", Title: "Unlabeled severity"},
		{Severity: "Medium", Title: "Missing error check"},
		{Severity: "Info", Title: "Consider a comment"},
	}

	got := make([]string, 0, len(findings))
	for _, finding := range blockingReviewFindings(findings) {
		got = append(got, finding.Title)
	}
	// An unlabeled severity stays blocking: a finding the harness cannot
	// classify must not be waved through as a nitpick.
	want := []string{"Data loss", "Unlabeled severity", "Missing error check"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockingReviewFindings() titles = %v, want %v", got, want)
	}
}

// A review cycle that only turns up advisory nitpicks must never fail the run:
// the nitpicks are reported in the review comment for a human and the pull
// request still leaves draft. The cycle may spend one otherwise-unused fix pass
// trying to repair them, and when that pass declines to change anything the
// cycle finishes successfully instead of reporting exhausted passes.
func TestRunFinalReviewCycleReleasesWhenOnlyAdvisoryFindingsRemain(t *testing.T) {
	runner := &cleanReviewCycleRunner{
		t:            t,
		reviewStdout: "**Positive**\n- Scoped change.\n\n**Negative**\n- [Low] main.go:12 - Rename the local variable.\n\n```json\n{\"status\":\"findings\",\"mergeReady\":false,\"summary\":\"nitpick only\",\"positives\":[\"Scoped change.\"],\"findings\":[{\"severity\":\"Low\",\"path\":\"main.go\",\"line\":12,\"title\":\"Rename the local variable\"}]}\n```",
	}
	h := New(runner)
	h.FinalReviewPasses = 3

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:        "https://github.com/acme/repo.git",
		Dir:        repoDir,
		RelDir:     "repo",
		Branch:     "feature",
		BaseBranch: "main",
		PRURL:      "https://github.com/acme/repo/pull/7",
		Changed:    true,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior."},
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if runner.reviews != 1 || runner.fixes != 1 || runner.comments != 1 {
		t.Fatalf("review/fix/comment counts = %d/%d/%d, want 1/1/1", runner.reviews, runner.fixes, runner.comments)
	}
	// The single advisory pass has to be handed the nitpick it exists to repair.
	if !strings.Contains(runner.fixPrompt, "[Low] main.go:12 - Rename the local variable") {
		t.Fatalf("advisory repair pass did not carry the advisory finding:\n%s", runner.fixPrompt)
	}
}

func TestRunFinalReviewCycleDisallowsMutatingToolsOnClaude(t *testing.T) {
	runner := &cleanReviewCycleRunner{t: t}
	h := New(runner)
	h.FinalReviewPasses = 1

	claudeRuntime, err := agentruntime.Resolve(agentruntime.HarnessClaude, "")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:        "https://github.com/acme/repo.git",
		Dir:        repoDir,
		RelDir:     "repo",
		Branch:     "feature",
		BaseBranch: "main",
		PRURL:      "https://github.com/acme/repo/pull/7",
		Changed:    true,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior."},
		repo,
		claudeRuntime,
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if len(runner.argv) != 1 {
		t.Fatalf("captured invocations = %d, want 1", len(runner.argv))
	}
	args := runner.argv[0]
	idx := -1
	for i, arg := range args {
		if arg == "--disallowedTools" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("args %v did not include --disallowedTools", args)
	}
	wantTools := []string{"Edit", "Write", "NotebookEdit", "Bash", "Agent", "Skill", "Workflow", "ReportFindings"}
	if got := args[idx+1 : idx+1+len(wantTools)]; !reflect.DeepEqual(got, wantTools) {
		t.Fatalf("disallowed tools = %v, want %v", got, wantTools)
	}
}

func TestRunFinalReviewCycleUsesReadOnlySandboxOnCodex(t *testing.T) {
	runner := &cleanReviewCycleRunner{t: t}
	h := New(runner)
	h.FinalReviewPasses = 1

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:        "https://github.com/acme/repo.git",
		Dir:        repoDir,
		RelDir:     "repo",
		Branch:     "feature",
		BaseBranch: "main",
		PRURL:      "https://github.com/acme/repo/pull/7",
		Changed:    true,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior."},
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if len(runner.argv) != 1 {
		t.Fatalf("captured invocations = %d, want 1", len(runner.argv))
	}
	args := runner.argv[0]
	idx := -1
	for i, arg := range args {
		if arg == "--sandbox" {
			idx = i
			break
		}
	}
	if idx == -1 || idx+1 >= len(args) {
		t.Fatalf("args %v did not include --sandbox", args)
	}
	if got := args[idx+1]; got != "read-only" {
		t.Fatalf("sandbox mode = %q, want %q (review pass must not retain workspace-write access)", got, "read-only")
	}
	for _, arg := range args {
		if arg == "--add-dir" || strings.Contains(arg, "sandbox_workspace_write") {
			t.Fatalf("args %v unexpectedly retained workspace-write configuration during a read-only review pass", args)
		}
	}
}

// finalFindingReviewCycleRunner answers review passes with one finding whose
// findingSeverity defaults to Medium, which blocks the pull request. Setting an
// advisory severity instead drives the non-blocking path through the same
// command script.
type finalFindingReviewCycleRunner struct {
	t               *testing.T
	findingSeverity string
	findingsPass    int
	neverClean      bool
	reviews         int
	fixes           int
	statusCalls     int
	comments        int
	checks          int
	fixPrompt       string
}

func (r *finalFindingReviewCycleRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	r.t.Helper()
	switch cmd.Name {
	case "codex":
		if strings.Contains(cmd.Stdin, "Automated review repair pass") {
			r.fixes++
			r.fixPrompt = cmd.Stdin
			return execx.Result{Stdout: "Fixed and tested."}, nil
		}
		r.reviews++
		if r.neverClean || r.reviews == r.findingsPass {
			severity := r.findingSeverity
			if severity == "" {
				severity = "Medium"
			}
			return execx.Result{Stdout: fmt.Sprintf("**Positive**\n- Scoped change.\n\n**Negative**\n- [%s] main.go:12 - Handle the error.\n\n```json\n{\"status\":\"findings\",\"mergeReady\":false,\"summary\":\"fix needed\",\"positives\":[\"Scoped change.\"],\"findings\":[{\"severity\":%q,\"path\":\"main.go\",\"line\":12,\"title\":\"Handle the error\"}]}\n```", severity, severity)}, nil
		}
		return execx.Result{Stdout: "**Positive**\n- Scoped change.\n\n**Negative**\n- No material issues found.\n\n```json\n{\"status\":\"clean\",\"mergeReady\":true,\"summary\":\"clean\",\"positives\":[\"Scoped change.\"],\"findings\":[]}\n```"}, nil
	case "git":
		if len(cmd.Args) == 0 {
			break
		}
		switch cmd.Args[0] {
		case "rev-parse":
			return execx.Result{Stdout: "head-sha\n"}, nil
		case "status":
			r.statusCalls++
			if r.statusCalls == 2 {
				return execx.Result{Stdout: "## feature\n M main.go\n"}, nil
			}
			return execx.Result{Stdout: "## feature\n"}, nil
		case "fetch", "merge", "add", "commit", "push":
			return execx.Result{}, nil
		case "diff":
			return execx.Result{Stdout: "diff --git a/main.go b/main.go\n"}, nil
		case "ls-remote":
			return execx.Result{Stdout: "head-sha\trefs/heads/feature\n"}, nil
		}
	case "gh":
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "view" {
			return execx.Result{Stdout: `{"number":7,"title":"Change","url":"https://github.com/acme/repo/pull/7","state":"OPEN","baseRefName":"main","headRefName":"feature","headRefOid":"head-sha","author":{"login":"dev"}}`}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "review" {
			r.comments++
			return execx.Result{}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "list" {
			return execx.Result{Stdout: `[{"url":"https://github.com/acme/repo/pull/7"}]`}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "checks" {
			r.checks++
			return execx.Result{Stdout: "tests\tpass\n"}, nil
		}
		if len(cmd.Args) >= 1 && cmd.Args[0] == "api" {
			return execx.Result{Stdout: "[]"}, nil
		}
	}
	r.t.Fatalf("unexpected command: %+v", cmd)
	return execx.Result{}, fmt.Errorf("unexpected command")
}

func TestRunFinalReviewCycleConfirmsFinalPassFixBeforeRelease(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, findingsPass: 1}
	h := New(runner)
	h.FinalReviewPasses = 1
	h.PRChecksWatchTimeout = -1

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:                "https://github.com/acme/repo.git",
		Dir:                repoDir,
		RelDir:             "repo",
		Branch:             "feature",
		BaseBranch:         "main",
		PRURL:              "https://github.com/acme/repo/pull/7",
		Changed:            true,
		WriteAccessChecked: true,
		WriteAccessAllowed: true,
		PushRemote:         publishRemoteOrigin,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "fix review"},
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	// A fix applied on the final configured pass must be confirmed clean by
	// an extra review before the cycle reports success, so there are two
	// review calls even though FinalReviewPasses is 1.
	if runner.reviews != 2 || runner.fixes != 1 {
		t.Fatalf("review/fix calls = %d/%d, want 2/1", runner.reviews, runner.fixes)
	}
	if runner.comments != 2 || runner.checks != 1 {
		t.Fatalf("comment/check counts = %d/%d, want 2/1", runner.comments, runner.checks)
	}
	for _, want := range []string{"resolve", "Implement the requested behavior.", "main.go:12", "complete repair input"} {
		if !strings.Contains(strings.ToLower(runner.fixPrompt), strings.ToLower(want)) {
			t.Fatalf("fix prompt missing %q", want)
		}
	}
}

func TestRunFinalReviewCycleFailsWhenFinalConfirmationStillHasFindings(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, neverClean: true}
	h := New(runner)
	h.FinalReviewPasses = 1
	h.PRChecksWatchTimeout = -1

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:                "https://github.com/acme/repo.git",
		Dir:                repoDir,
		RelDir:             "repo",
		Branch:             "feature",
		BaseBranch:         "main",
		PRURL:              "https://github.com/acme/repo/pull/7",
		Changed:            true,
		WriteAccessChecked: true,
		WriteAccessAllowed: true,
		PushRemote:         publishRemoteOrigin,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "fix review"},
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	// The PR must never be released from draft when findings persist after
	// every configured fix pass has been exhausted.
	if err == nil || exitCode != ExitCodex || stage != "review" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want failure leaving PR in draft", exitCode, stage, err)
	}
	if runner.reviews != 2 || runner.fixes != 1 {
		t.Fatalf("review/fix calls = %d/%d, want 2/1", runner.reviews, runner.fixes)
	}
}

func TestRunFinalReviewCycleContinuesAfterFixUntilClean(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, findingsPass: 1}
	h := New(runner)
	h.FinalReviewPasses = 3
	h.PRChecksWatchTimeout = -1

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:                "https://github.com/acme/repo.git",
		Dir:                repoDir,
		RelDir:             "repo",
		Branch:             "feature",
		BaseBranch:         "main",
		PRURL:              "https://github.com/acme/repo/pull/7",
		Changed:            true,
		WriteAccessChecked: true,
		WriteAccessAllowed: true,
		PushRemote:         publishRemoteOrigin,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(), config.Config{Prompt: "Implement the requested behavior."}, repo,
		agentruntime.Default(), repoDir, codexRunOptions{}, "", "", "repo", "codex", false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if runner.reviews != 2 || runner.fixes != 1 || runner.comments != 2 || runner.checks != 1 {
		t.Fatalf("reviews/fixes/comments/checks = %d/%d/%d/%d, want 2/1/2/1", runner.reviews, runner.fixes, runner.comments, runner.checks)
	}
}

// The advisory fix pass is offered at most once per cycle. A review that keeps
// reporting the same nitpick after that pass landed must release the pull
// request rather than spending the rest of the budget on cosmetic churn.
func TestRunFinalReviewCycleSpendsAtMostOneAdvisoryFixPass(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, neverClean: true, findingSeverity: "Low"}
	h := New(runner)
	h.FinalReviewPasses = 3
	h.PRChecksWatchTimeout = -1

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:                "https://github.com/acme/repo.git",
		Dir:                repoDir,
		RelDir:             "repo",
		Branch:             "feature",
		BaseBranch:         "main",
		PRURL:              "https://github.com/acme/repo/pull/7",
		Changed:            true,
		WriteAccessChecked: true,
		WriteAccessAllowed: true,
		PushRemote:         publishRemoteOrigin,
	}

	exitCode, stage, err := h.runFinalReviewCycle(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "fix review"},
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	// Advisory findings never block, so the second pass releases the pull
	// request instead of failing it the way surviving blocking findings do.
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if runner.reviews != 2 || runner.fixes != 1 {
		t.Fatalf("review/fix calls = %d/%d, want 2/1 (the advisory budget is spent once)", runner.reviews, runner.fixes)
	}
}

// TestProcessChangedRepoReleasesPRFromDraftAfterCleanFinalReviewCycle exercises
// the end-to-end wiring in processChangedRepo: once the normal commit/push/PR
// flow finishes, a configured final review cycle must run, and a clean cycle
// must release the auto-drafted PR instead of leaving it in draft.
func TestProcessChangedRepoReleasesPRFromDraftAfterCleanFinalReviewCycle(t *testing.T) {
	runner := &cleanReviewCycleRunner{t: t}
	h := New(runner)
	h.FinalReviewPasses = 1

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:                "https://github.com/acme/repo.git",
		Dir:                repoDir,
		RelDir:             "repo",
		Branch:             "feature",
		BaseBranch:         "main",
		PRURL:              "https://github.com/acme/repo/pull/7",
		Changed:            true,
		CreateWorkBranch:   true,
		WriteAccessChecked: true,
		WriteAccessAllowed: true,
		PushRemote:         publishRemoteOrigin,
	}

	cfg := config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "do the work"}

	exitCode, stage, err := h.processChangedRepo(
		context.Background(),
		cfg,
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("processChangedRepo() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if runner.reviews != 1 || runner.comments != 1 {
		t.Fatalf("review/comment counts = %d/%d, want 1/1", runner.reviews, runner.comments)
	}
	if runner.released != 1 {
		t.Fatalf("released count = %d, want 1 (PR must be released once the final review cycle is clean)", runner.released)
	}
}

// TestProcessChangedRepoLeavesPRInDraftWhenFinalReviewCycleFails exercises the
// same wiring for the failure path: when the final review cycle cannot reach
// a clean pass, processChangedRepo must surface the failure and must never
// call through to release the PR from draft.
func TestProcessChangedRepoLeavesPRInDraftWhenFinalReviewCycleFails(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, neverClean: true}
	h := New(runner)
	h.FinalReviewPasses = 1
	h.PRChecksWatchTimeout = -1

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo := &repoWorkspace{
		URL:                "https://github.com/acme/repo.git",
		Dir:                repoDir,
		RelDir:             "repo",
		Branch:             "feature",
		BaseBranch:         "main",
		PRURL:              "https://github.com/acme/repo/pull/7",
		Changed:            true,
		CreateWorkBranch:   true,
		WriteAccessChecked: true,
		WriteAccessAllowed: true,
		PushRemote:         publishRemoteOrigin,
	}

	cfg := config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "do the work"}

	exitCode, stage, err := h.processChangedRepo(
		context.Background(),
		cfg,
		repo,
		agentruntime.Default(),
		repoDir,
		codexRunOptions{},
		"",
		"",
		"repo",
		"codex",
		false,
	)
	if err == nil || exitCode != ExitCodex || stage != "review" {
		t.Fatalf("processChangedRepo() = (%d, %q, %v), want final review cycle failure", exitCode, stage, err)
	}
	if runner.reviews != 2 || runner.fixes != 1 {
		t.Fatalf("review/fix calls = %d/%d, want 2/1", runner.reviews, runner.fixes)
	}
}
