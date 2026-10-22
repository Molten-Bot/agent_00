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

type cleanReviewCycleRunner struct {
	t        *testing.T
	mu       sync.Mutex
	reviews  int
	comments int
	prompts  []string
	argv     [][]string
}

func (r *cleanReviewCycleRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	switch cmd.Name {
	case "codex":
		r.reviews++
		r.prompts = append(r.prompts, cmd.Stdin)
		r.argv = append(r.argv, append([]string(nil), cmd.Args...))
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
		case "fetch":
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
	wantTools := []string{"Edit", "Write", "NotebookEdit"}
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

type finalFindingReviewCycleRunner struct {
	t            *testing.T
	findingsPass int
	neverClean   bool
	reviews      int
	fixes        int
	statusCalls  int
	comments     int
	checks       int
	fixPrompt    string
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
			return execx.Result{Stdout: "**Positive**\n- Scoped change.\n\n**Negative**\n- [Medium] main.go:12 - Handle the error.\n\n```json\n{\"status\":\"findings\",\"mergeReady\":false,\"summary\":\"fix needed\",\"positives\":[\"Scoped change.\"],\"findings\":[{\"severity\":\"Medium\",\"path\":\"main.go\",\"line\":12,\"title\":\"Handle the error\"}]}\n```"}, nil
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
		case "fetch", "add", "commit", "push":
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
