package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

type cleanReviewCycleRunner struct {
	t                *testing.T
	mu               sync.Mutex
	reviews          int
	comments         int
	checks           int
	released         int
	prompts          []string
	argv             [][]string
	emptyNetDiff     bool
	mergeAfterReview bool
	api              *reviewAPIStub
}

func TestFinalReviewModeUsesConfiguredReviewCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		passes int
		want   string
	}{
		{passes: 0, want: "off"},
		{passes: 1, want: "custom"},
		{passes: 2, want: "low"},
		{passes: 3, want: "custom"},
		{passes: 4, want: "custom"},
		{passes: 5, want: "medium"},
		{passes: 6, want: "custom"},
		{passes: 7, want: "custom"},
		{passes: 8, want: "custom"},
		{passes: 9, want: "custom"},
		{passes: 10, want: "high"},
	}
	for _, tt := range tests {
		if got := finalReviewMode(tt.passes); got != tt.want {
			t.Errorf("finalReviewMode(%d) = %q, want %q", tt.passes, got, tt.want)
		}
	}
}

func TestRunLogsCustomWorkflowReviewConfig(t *testing.T) {
	t.Parallel()

	preflightErr := errors.New("stop after configuration")
	h := New(&fakeRunner{t: t, exps: []expectedRun{
		{cmd: execx.Command{Name: "git", Args: []string{"--version"}}, err: preflightErr},
	}})
	var logs []string
	h.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	cfg := sampleConfig()
	cfg.Workflow = &config.WorkflowRunConfig{ID: "workflow", EventID: "event", ReviewCycles: 1}

	result := h.Run(t.Context(), cfg)
	if result.ExitCode != ExitPreflight {
		t.Fatalf("Run() exit = %d, err = %v, want preflight failure", result.ExitCode, result.Err)
	}
	if want := "stage=config status=ok review_mode=custom review_total=1"; !slices.Contains(logs, want) {
		t.Fatalf("logs missing %q: %v", want, logs)
	}
}

func (r *cleanReviewCycleRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.api == nil {
		r.api = &reviewAPIStub{t: r.t, head: "head-sha"}
	}
	if res, ok, err := r.api.handle(cmd); ok {
		r.comments = r.api.posts
		return res, err
	}

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
		case "fetch", "merge", "add", "commit", "push":
			return execx.Result{}, nil
		case "diff":
			for _, arg := range cmd.Args {
				if arg == "--name-only" {
					if r.emptyNetDiff {
						return execx.Result{}, nil
					}
					return execx.Result{Stdout: "main.go\n"}, nil
				}
			}
			return execx.Result{Stdout: "diff --git a/main.go b/main.go\n"}, nil
		}
	case "gh":
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "view" {
			if r.mergeAfterReview && slices.Contains(cmd.Args, "url,title,state,mergedAt,baseRefName,headRefName,headRefOid,headRepository,headRepositoryOwner") {
				return execx.Result{Stdout: `{"url":"https://github.com/acme/repo/pull/7","state":"MERGED","mergedAt":"2026-11-18T12:00:00Z","headRefName":"feature"}`}, nil
			}
			return execx.Result{Stdout: `{"number":7,"title":"Change","url":"https://github.com/acme/repo/pull/7","state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main","headRefName":"feature","headRefOid":"head-sha","author":{"login":"dev"}}`}, nil
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
	for _, passes := range []int{1, 3} {
		t.Run(strconv.Itoa(passes), func(t *testing.T) {
			testRunFinalReviewCycleStopsAfterCleanPass(t, passes)
		})
	}
}

func testRunFinalReviewCycleStopsAfterCleanPass(t *testing.T, passes int) {
	t.Helper()
	runner := &cleanReviewCycleRunner{t: t}
	h := New(runner)
	h.FinalReviewPasses = passes
	var logs []string
	h.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
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
		config.Config{Prompt: "Implement the requested behavior. Post progress to the PR."},
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
	for _, status := range []string{"start", "ok"} {
		want := fmt.Sprintf("stage=review status=%s action=pass review_mode=custom pass=1 total=%d ", status, passes)
		if !slices.ContainsFunc(logs, func(line string) bool { return strings.HasPrefix(line, want) }) {
			t.Fatalf("logs missing prefix %q: %v", want, logs)
		}
	}
	for i, prompt := range runner.prompts {
		if !strings.Contains(prompt, "The harness alone publishes the review") || !strings.Contains(prompt, fmt.Sprintf("Post-task review pass %d/%d", i+1, passes)) {
			t.Fatalf("review prompt %d did not include skill and pass context", i+1)
		}
	}
}

func TestRunFinalReviewCycleStopsWhenPullRequestMergesDuringReview(t *testing.T) {
	runner := &cleanReviewCycleRunner{t: t, mergeAfterReview: true}
	var logs []string
	h := New(runner)
	h.FinalReviewPasses = 3
	h.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
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
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want successful early stop", exitCode, stage, err)
	}
	if runner.reviews != 1 || runner.comments != 0 {
		t.Fatalf("review/comment counts = %d/%d, want 1/0 after merge", runner.reviews, runner.comments)
	}
	if !repo.ReviewStoppedOnMerge {
		t.Fatal("ReviewStoppedOnMerge = false, want true")
	}
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "status=warn action=stop reason=pr_merged") {
		t.Fatalf("logs missing merged-during-review warning: %s", joined)
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
	wantTools := []string{"mcp__*", "Edit", "Write", "NotebookEdit", "Bash", "Agent", "Skill", "Workflow", "ReportFindings"}
	if got := args[idx+1 : len(args)-1]; !slices.Equal(got, wantTools) {
		t.Fatalf("disallowed tools = %v, want %v", got, wantTools)
	}
	if tools, ok := flagValue(args, "--tools"); !ok || tools != "Read,Glob,Grep" || !slices.Contains(args, "--safe-mode") {
		t.Fatalf("Claude review tools were not limited to local inspection: %v", args)
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
		if arg == "-c" && i+1 < len(args) && args[i+1] == `default_permissions="agent00_review"` {
			idx = i
			break
		}
	}
	if idx == -1 || idx+1 >= len(args) {
		t.Fatalf("args %v did not select the review permission profile", args)
	}
	if !slices.ContainsFunc(args, func(arg string) bool {
		return strings.HasPrefix(arg, "permissions.agent00_review=") && strings.HasSuffix(arg, "network={enabled=false}}")
	}) || !slices.Contains(args, "mcp_servers={}") {
		t.Fatalf("review retained shell networking or configured MCP servers: %v", args)
	}
	for _, feature := range []string{"apps", "plugins", "hooks", "browser_use", "computer_use"} {
		if !containsSequence(args, []string{"--disable", feature}) {
			t.Fatalf("review invocation did not disable %s: %v", feature, args)
		}
	}
	for _, arg := range args {
		if arg == "--add-dir" || strings.Contains(arg, "sandbox_workspace_write") {
			t.Fatalf("args %v unexpectedly retained workspace-write configuration during a read-only review pass", args)
		}
	}
}

type finalFindingReviewCycleRunner struct {
	t              *testing.T
	findingsPass   int
	neverClean     bool
	historyOnly    bool
	reviews        int
	fixes          int
	statusCalls    int
	comments       int
	checks         int
	drafted        int
	released       int
	fixPrompt      string
	emptyNetDiff   bool
	modelProbes    int
	agentCalls     []execx.Command
	repairLagReads int
	remoteMoved    bool
	pushedRepair   bool
	api            *reviewAPIStub
}

func (r *finalFindingReviewCycleRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	r.t.Helper()
	if r.api == nil {
		r.api = &reviewAPIStub{t: r.t, head: "head-sha"}
	}
	if res, ok, err := r.api.handle(cmd); ok {
		if len(cmd.Args) > 2 && cmd.Args[2] == "POST" {
			r.comments++
		}
		return res, err
	}
	switch cmd.Name {
	case "claude":
		if reflect.DeepEqual(cmd.Args, []string{"--version"}) {
			r.modelProbes++
			return execx.Result{Stdout: "claude 1.2.3"}, nil
		}
		fallthrough
	case "codex":
		r.agentCalls = append(r.agentCalls, cmd)
		if strings.Contains(cmd.Stdin, "Automated review repair pass") {
			r.fixes++
			r.fixPrompt = cmd.Stdin
			return execx.Result{Stdout: "Fixed and tested."}, nil
		}
		r.reviews++
		if r.neverClean || r.reviews == r.findingsPass {
			if r.historyOnly {
				return execx.Result{Stdout: "**Positive**\n- Scoped change.\n\n**Negative**\n- [High] main.go:12 - Merge the base branch.\n\n```json\n{\"status\":\"findings\",\"mergeReady\":false,\"summary\":\"history update needed\",\"positives\":[\"Scoped change.\"],\"findings\":[{\"severity\":\"High\",\"path\":\"main.go\",\"line\":12,\"title\":\"Merge the base branch\",\"requiresBranchHistoryChange\":true}]}\n```"}, nil
			}
			return execx.Result{Stdout: "**Positive**\n- Scoped change.\n\n**Negative**\n- [Medium] main.go:12 - Handle the error.\n\n```json\n{\"status\":\"findings\",\"mergeReady\":false,\"summary\":\"fix needed\",\"positives\":[\"Scoped change.\"],\"findings\":[{\"severity\":\"Medium\",\"path\":\"main.go\",\"line\":12,\"title\":\"Handle the error\"}]}\n```"}, nil
		}
		return execx.Result{Stdout: "**Positive**\n- Scoped change.\n\n**Negative**\n- No material issues found.\n\n```json\n{\"status\":\"clean\",\"mergeReady\":true,\"summary\":\"clean\",\"positives\":[\"Scoped change.\"],\"findings\":[]}\n```"}, nil
	case "git":
		if len(cmd.Args) == 0 {
			break
		}
		switch cmd.Args[0] {
		case "rev-parse":
			if r.pushedRepair {
				return execx.Result{Stdout: "repaired-sha\n"}, nil
			}
			return execx.Result{Stdout: "head-sha\n"}, nil
		case "status":
			r.statusCalls++
			if r.statusCalls == 2 {
				return execx.Result{Stdout: "## feature\n M main.go\n"}, nil
			}
			return execx.Result{Stdout: "## feature\n"}, nil
		case "push":
			if r.fixes > 0 {
				r.pushedRepair = true
				r.api.head = "repaired-sha"
				if r.remoteMoved {
					r.api.head = "concurrent-sha"
				}
			}
			return execx.Result{}, nil
		case "fetch", "merge", "add", "commit":
			return execx.Result{}, nil
		case "diff":
			for _, arg := range cmd.Args {
				if arg == "--name-only" && r.emptyNetDiff {
					return execx.Result{}, nil
				}
			}
			return execx.Result{Stdout: "diff --git a/main.go b/main.go\n"}, nil
		case "ls-remote":
			return execx.Result{Stdout: r.api.head + "\trefs/heads/feature\n"}, nil
		}
	case "gh":
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "view" {
			head := r.api.head
			if r.pushedRepair && r.remoteMoved {
				head = "repaired-sha"
			}
			if r.pushedRepair && cmd.Args[len(cmd.Args)-1] == "number,title,body,url,state,isDraft,baseRefName,headRefName,headRefOid,author" && r.repairLagReads > 0 {
				r.repairLagReads--
				head = "head-sha"
			}
			if slices.Contains(cmd.Args, "state,headRefOid,statusCheckRollup") {
				return execx.Result{Stdout: fmt.Sprintf(`{"state":"OPEN","headRefOid":%q,"statusCheckRollup":[{"name":"CI","conclusion":"SUCCESS"}]}`, head)}, nil
			}
			return execx.Result{Stdout: fmt.Sprintf(`{"id":"PR_id","number":7,"title":"Change","url":"https://github.com/acme/repo/pull/7","state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main","headRefName":"feature","headRefOid":%q,"author":{"login":"dev"}}`, head)}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "list" {
			return execx.Result{Stdout: `[{"url":"https://github.com/acme/repo/pull/7"}]`}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "checks" {
			r.checks++
			return execx.Result{Stdout: "tests\tpass\n"}, nil
		}
		if len(cmd.Args) >= 2 && cmd.Args[0] == "pr" && cmd.Args[1] == "ready" {
			if slices.Contains(cmd.Args, "--undo") {
				r.drafted++
			} else {
				r.released++
			}
			return execx.Result{}, nil
		}
		if len(cmd.Args) >= 1 && cmd.Args[0] == "api" {
			args := strings.Join(cmd.Args, " ")
			switch {
			case strings.Contains(args, "/protection/required_status_checks"):
				return execx.Result{Stdout: `{"contexts":["CI"]}`}, nil
			case strings.Contains(args, "isRequired(pullRequestId: $id)"):
				return execx.Result{Stdout: checkRollupPageJSON("feature", r.api.head, r.api.head,
					`[{"name":"CI","conclusion":"SUCCESS","isRequired":true}]`, false, "")}, nil
			}
			return execx.Result{Stdout: "[]"}, nil
		}
	}
	r.t.Fatalf("unexpected command: %+v", cmd)
	return execx.Result{}, fmt.Errorf("unexpected command")
}

func TestRunFinalReviewCycleDoesNotSendBranchHistoryFindingToRepairAgent(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, findingsPass: 1, historyOnly: true}
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
	if err == nil || exitCode != ExitCodex || stage != "review" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want blocked review", exitCode, stage, err)
	}
	if runner.reviews != 1 || runner.fixes != 0 {
		t.Fatalf("review/fix calls = %d/%d, want 1/0", runner.reviews, runner.fixes)
	}
	if runner.comments != 0 || repo.ReviewReceipt != nil {
		t.Fatalf("history finding published a clean review: %d writes, receipt %+v", runner.comments, repo.ReviewReceipt)
	}
}

func TestRunFinalReviewCycleConfirmsFinalPassFixBeforeRelease(t *testing.T) {
	for _, tc := range []struct {
		name            string
		lagReads, waits int
		remoteMoved     bool
		wantErr         error
	}{
		{name: "transient lag", lagReads: 1, waits: 1},
		{name: "persistent lag", lagReads: 10, waits: 5, wantErr: ErrReviewPending},
		{name: "concurrent update while metadata shows the repair", remoteMoved: true, wantErr: ErrReviewHeadChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &finalFindingReviewCycleRunner{t: t, findingsPass: 1, repairLagReads: tc.lagReads, remoteMoved: tc.remoteMoved}
			h := New(runner)
			h.FinalReviewPasses = 1
			h.PRChecksWatchTimeout = -1
			waits := 0
			h.Sleep = func(context.Context, time.Duration) error { waits++; return nil }

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
			if waits != tc.waits {
				t.Fatalf("waits = %d, want %d", waits, tc.waits)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || exitCode != ExitPR || stage != "review" ||
					runner.reviews != 1 || runner.comments != 1 || repo.ReviewReceipt != nil {
					t.Fatalf("expected review to stay gated without new evidence, got (%d, %q, %v), reviews=%d, receipt=%+v", exitCode, stage, err, runner.reviews, repo.ReviewReceipt)
				}
				return
			}
			if repo.ReviewReceipt == nil || repo.ReviewReceipt.Head != "repaired-sha" {
				t.Fatalf("missing clean receipt on repaired commit: %+v", repo.ReviewReceipt)
			}
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
		})
	}
}

func TestRunFinalReviewCycleReportsRemainingFindingsAfterFinalConfirmation(t *testing.T) {
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
	// Exhausting the repair budget is a controlled incomplete-review outcome,
	// not an agent execution failure. The caller keeps the PR in draft.
	if !errors.Is(err, errFinalReviewFindingsRemain) || exitCode != ExitSuccess || stage != "review" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want remaining-findings outcome", exitCode, stage, err)
	}
	if runner.reviews != 2 || runner.fixes != 1 {
		t.Fatalf("review/fix calls = %d/%d, want 2/1", runner.reviews, runner.fixes)
	}
}

func TestManagedFinalReviewKeepsExhaustedDraftPending(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, neverClean: true}
	h := New(runner)
	h.FinalReviewPasses = 1
	h.PRChecksWatchTimeout = -1
	dir := t.TempDir()
	repo := &repoWorkspace{
		URL: "https://github.com/acme/repo", Dir: dir, RelDir: "repo",
		Branch: "feature", BaseBranch: "main", PRURL: "https://github.com/acme/repo/pull/7",
		Changed: true, ResumeWorkflowPR: true, WriteAccessChecked: true, WriteAccessAllowed: true,
	}
	cfg := config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "fix review",
		Workflow: &config.WorkflowRunConfig{DefaultApplied: true, ReviewCycles: 1}}
	code, stage, err := h.finalizePullRequestReview(t.Context(), cfg, repo, agentruntime.Default(), dir,
		codexRunOptions{}, "", "", "repo", "codex", false)
	if code != ExitPR || stage != "review" || !errors.Is(err, ErrReviewPending) ||
		!errors.Is(err, errFinalReviewFindingsRemain) || runner.drafted != 1 || runner.released != 0 {
		t.Fatalf("review = (%d, %q, %v), draft/ready = %d/%d", code, stage, err, runner.drafted, runner.released)
	}
}

func TestAppManagedMergeAfterIndependentReviewAndRepair(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, findingsPass: 1, repairLagReads: 1}
	h := New(runner)
	h.FinalReviewPasses = 10
	h.PRChecksWatchTimeout = -1
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.resolveTaskModel = func() (agentruntime.ModelSelection, error) {
		return agentruntime.ModelSelection{Model: "sonnet"}, nil
	}
	runtime, err := agentruntime.Resolve(agentruntime.HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}

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

	const reviewInstructions = "Check error handling and recovery."
	w := workflow.Definition{
		ID: "review-delivery", Name: "Review delivery", Repository: "acme/repo",
		Enabled: true, IncludeBacklog: true, ReviewCycles: h.FinalReviewPasses,
		ReviewAgent: config.ReviewAgentConfig{Complexity: "easy", Instructions: reviewInstructions},
		Trigger:     workflow.Trigger{Event: "issue.opened"},
		Filters:     workflow.Filters{Mode: "all"},
		Task:        workflow.Task{Kind: "implement", Instructions: "Implement the requested behavior.", IssueLinkMode: "reference"},
		Merge:       workflow.Merge{Enabled: true, Method: "squash", DeleteBranch: true},
	}
	cfg := config.Config{Prompt: "Implement the requested behavior.", Workflow: &config.WorkflowRunConfig{
		PolicyHash: w.ExecutionPolicyHash(), DefaultApplied: true, ReviewCycles: 10,
		ReviewAgent: config.ReviewAgentConfig{Instructions: reviewInstructions, Complexity: "easy"},
	}}
	exitCode, stage, err := h.runFinalReviewCycle(
		t.Context(), cfg, repo, runtime, repoDir, codexRunOptions{}, "", "", "repo", "claude", false,
	)
	if err != nil || exitCode != ExitSuccess || stage != "" {
		t.Fatalf("runFinalReviewCycle() = (%d, %q, %v), want success", exitCode, stage, err)
	}
	if runner.reviews != 2 || runner.fixes != 1 || runner.comments != 2 || runner.checks != 1 {
		t.Fatalf("reviews/fixes/comments/checks = %d/%d/%d/%d, want 2/1/2/1", runner.reviews, runner.fixes, runner.comments, runner.checks)
	}
	if runner.modelProbes != 1 {
		t.Fatalf("review model probes = %d, want 1", runner.modelProbes)
	}
	for i, wantModel := range []string{"haiku", "sonnet", "haiku"} {
		call := runner.agentCalls[i]
		modelIndex := slices.Index(call.Args, "--model")
		if modelIndex < 0 || call.Args[modelIndex+1] != wantModel {
			t.Fatalf("agent call %d did not use model %q: %v", i+1, wantModel, call.Args)
		}
		if hasInstructions := strings.Contains(call.Stdin, reviewInstructions); hasInstructions != (i != 1) {
			t.Fatalf("agent call %d received review instructions = %t", i+1, hasInstructions)
		}
	}
	receipt := repo.ReviewReceipt
	if receipt == nil || !receipt.Clean() || receipt.Policy.FixPasses != 10 || receipt.Policy.Complexity != "easy" {
		t.Fatalf("final review did not capture the delivery policy: %+v", receipt)
	}
	if err := h.completeCleanReviewGate(t.Context(), cfg, repo, true); err != nil || runner.released != 1 {
		t.Fatalf("ready calls = %d, error = %v", runner.released, err)
	}

	merges := 0
	gh := workflow.GitHub{Runner: sessionRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		args := strings.Join(cmd.Args, " ")
		switch {
		case strings.Contains(args, "statusCheckRollup"):
			return execx.Result{Stdout: `{"number":7,"state":"OPEN","headRefOid":"repaired-sha","statusCheckRollup":[{"name":"CI","conclusion":"SUCCESS"}]}`}, nil
		case strings.Contains(args, "closingIssuesReferences"):
			return execx.Result{Stdout: "0"}, nil
		case strings.Contains(args, "reviewThreads(first:100"):
			return execx.Result{Stdout: `{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":"repaired-sha","isMergeQueueEnabled":false,"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`}, nil
		case strings.HasPrefix(args, "pr merge "):
			want := []string{"pr", "merge", repo.PRURL, "--repo", w.Repository, "--squash", "--match-head-commit", receipt.Head, "--delete-branch"}
			if !slices.Equal(cmd.Args, want) {
				t.Fatalf("merge args = %v, want %v", cmd.Args, want)
			}
			merges++
			return execx.Result{}, nil
		case strings.HasSuffix(args, "--json state") && merges == 1:
			return execx.Result{Stdout: `{"state":"MERGED"}`}, nil
		default:
			return runner.Run(ctx, cmd)
		}
	})}
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	defaultExecution := &workflow.ExecutionPolicy{ReviewCycles: 10, ReviewAgent: config.ReviewAgentConfig{Complexity: "easy"}}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, DefaultExecution: defaultExecution, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	engine := &workflow.Engine{
		Store: store, StatePath: filepath.Join(t.TempDir(), "state.json"),
		Enabled: func() bool { return true }, Allowed: func(string) bool { return true },
		Fetch: func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) {
			return []workflow.Item{{Number: 1, Kind: "issue", Repository: w.Repository, Open: true}}, nil
		},
		Submit:    func(context.Context, workflow.Run) (string, error) { return "delivery-task", nil },
		Status:    func(string) workflow.Outcome { return workflow.Outcome{Status: "running"} },
		InspectPR: gh.Inspect, Merge: gh.Merge,
	}
	if err := engine.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	runs, err := engine.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("workflow activation = %+v, %v", runs, err)
	}
	if err := engine.Complete(runs[0].ID, workflow.Outcome{Status: "succeeded", PRURL: repo.PRURL, ReviewReceipt: receipt}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := engine.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if merges != 1 {
		t.Fatalf("app invoked gh pr merge %d times, want 1", merges)
	}
}

// TestProcessChangedRepoReleasesPRFromDraftAfterCleanFinalReviewCycle exercises
// the end-to-end wiring in processChangedRepo: once the normal commit/push/PR
// flow finishes, a configured final review cycle must run, and a clean cycle
// must release the auto-drafted PR instead of leaving it in draft.
func TestProcessChangedRepoReleasesPRFromDraftAfterCleanFinalReviewCycle(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		writeErr   error
		wantReady  int
	}{
		{name: "acknowledged", head: "head-sha", wantReady: 1},
		{name: "unacknowledged", head: "head-sha", writeErr: errors.New("upstream timeout")},
		{name: "head changed", head: "new-head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &cleanReviewCycleRunner{t: t, api: &reviewAPIStub{t: t, head: tc.head, writeErr: tc.writeErr}}
			h := New(runner)
			h.FinalReviewPasses = 1
			h.Sleep = func(context.Context, time.Duration) error { return nil }

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
			if (err == nil) != (tc.wantReady == 1) {
				t.Fatalf("processChangedRepo() = (%d, %q, %v)", exitCode, stage, err)
			}
			wantReviews := 1
			if tc.head != "head-sha" {
				wantReviews = 0
			}
			if runner.reviews != wantReviews || runner.comments != wantReviews {
				t.Fatalf("review/comment counts = %d/%d, want %d/%d", runner.reviews, runner.comments, wantReviews, wantReviews)
			}
			if runner.released != tc.wantReady {
				t.Fatalf("released count = %d, want %d", runner.released, tc.wantReady)
			}
		})
	}
}

func TestProcessChangedRepoRejectsEmptyNetDiffAfterReviewCycle(t *testing.T) {
	runner := &cleanReviewCycleRunner{t: t, emptyNetDiff: true}
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

	exitCode, stage, err := h.processChangedRepo(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "do the work"},
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
	if err == nil || exitCode != ExitPR || stage != "review" || !strings.Contains(err.Error(), "no net changes against the base branch") {
		t.Fatalf("processChangedRepo() = (%d, %q, %v), want empty-net-diff failure", exitCode, stage, err)
	}
	if runner.released != 0 {
		t.Fatalf("released count = %d, want 0 for an empty net diff", runner.released)
	}
}

func TestProcessChangedRepoSkipsPostReviewWorkWhenPullRequestMerged(t *testing.T) {
	runner := &cleanReviewCycleRunner{t: t, emptyNetDiff: true, mergeAfterReview: true}
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

	exitCode, stage, err := h.processChangedRepo(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "do the work"},
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
		t.Fatalf("processChangedRepo() = (%d, %q, %v), want successful merged-PR stop", exitCode, stage, err)
	}
	if runner.released != 0 {
		t.Fatalf("released count = %d, want 0 after PR merged", runner.released)
	}
}

// TestProcessChangedRepoLeavesPRInDraftWhenFinalReviewFindingsRemain exercises
// the bounded-review outcome: unresolved findings remain visible on the draft
// PR without turning completed task work into an agent_00 failure follow-up.
func TestProcessChangedRepoLeavesPRInDraftWhenFinalReviewFindingsRemain(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, neverClean: true}
	h := New(runner)
	h.FinalReviewPasses = 1
	h.PRChecksWatchTimeout = -1
	var logs []string
	h.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

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
		t.Fatalf("processChangedRepo() = (%d, %q, %v), want successful draft handoff", exitCode, stage, err)
	}
	if runner.reviews != 2 || runner.fixes != 1 {
		t.Fatalf("review/fix calls = %d/%d, want 2/1", runner.reviews, runner.fixes)
	}
	if runner.drafted != 1 {
		t.Fatalf("drafted count = %d, want 1 for unresolved findings", runner.drafted)
	}
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "status=warn action=leave_draft reason=fix_passes_exhausted") {
		t.Fatalf("logs missing exhausted-review warning: %s", joined)
	}
}

func TestProcessChangedRepoRejectsEmptyNetDiffWhenFinalReviewFindingsRemain(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, neverClean: true, emptyNetDiff: true}
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

	exitCode, stage, err := h.processChangedRepo(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "do the work"},
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
	if err == nil || exitCode != ExitPR || stage != "review" || !strings.Contains(err.Error(), "no net changes against the base branch") {
		t.Fatalf("processChangedRepo() = (%d, %q, %v), want empty-net-diff failure", exitCode, stage, err)
	}
	if runner.drafted != 1 {
		t.Fatalf("drafted count = %d, want 1 before rejecting empty net diff", runner.drafted)
	}
}

func TestCheckoutPublishedPRsKeepsEachOriginalBranch(t *testing.T) {
	t.Parallel()
	a := "https://github.com/acme/repo-a"
	b := "https://github.com/acme/repo-b"
	c := "https://github.com/acme/unchanged"
	prA, prB := a+"/pull/10", b+"/pull/20"
	dir := t.TempDir()
	repos := []repoWorkspace{
		{URL: a, Dir: filepath.Join(dir, "repo-a")},
		{URL: b, Dir: filepath.Join(dir, "repo-b")},
		{URL: c, Dir: filepath.Join(dir, "unchanged")},
	}
	runner := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prStateViewCommand("", prA), res: execx.Result{Stdout: fmt.Sprintf(
			`{"url":%q,"state":"OPEN","baseRefName":"main","headRefName":"branch-a","headRefOid":"head-a"}`, prA)}},
		{cmd: cloneRepoCommand(a, "branch-a", repos[0].Dir)},
		{cmd: fetchBaseBranchCommand(repos[0].Dir, "branch-a")},
		{cmd: prStateViewCommand("", prB), res: execx.Result{Stdout: fmt.Sprintf(
			`{"url":%q,"state":"OPEN","baseRefName":"release","headRefName":"branch-b","headRefOid":"head-b"}`, prB)}},
		{cmd: cloneRepoCommand(b, "branch-b", repos[1].Dir)},
		{cmd: fetchBaseBranchCommand(repos[1].Dir, "branch-b")},
	}}
	h := New(runner)
	got, code, stage, err := h.checkoutPublishedPRs(t.Context(), config.Config{}, repos, []string{prA, prB})
	if err != nil || len(runner.exps) != 0 || len(got) != 2 {
		t.Fatalf("checkout = (%+v, %d, %q, %v), commands left = %d", got, code, stage, err, len(runner.exps))
	}
	for i, repo := range got {
		if !repo.RequireExistingPR || !repo.ResumeWorkflowPR || repo.CreateWorkBranch ||
			repo.PRURL != []string{prA, prB}[i] || repo.Branch != []string{"branch-a", "branch-b"}[i] ||
			repo.BaseBranch != []string{"main", "release"}[i] || repo.Dir != repos[i].Dir {
			t.Fatalf("checkout changed target %d: %+v", i, repo)
		}
	}
}

func TestMetadataResumeRejectsEmptyNetDiffAfterReviewCycle(t *testing.T) {
	runner := &finalFindingReviewCycleRunner{t: t, emptyNetDiff: true}
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
		CreateWorkBranch:   false,
		ResumeWorkflowPR:   true,
		WriteAccessChecked: true,
		WriteAccessAllowed: true,
		PushRemote:         publishRemoteOrigin,
	}

	exitCode, stage, err := h.processChangedRepo(
		context.Background(),
		config.Config{Prompt: "Implement the requested behavior.", CommitMessage: "do the work"},
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
	if err == nil || exitCode != ExitPR || stage != "review" || !strings.Contains(err.Error(), "no net changes against the base branch") {
		t.Fatalf("processChangedRepo() = (%d, %q, %v), want empty-net-diff failure", exitCode, stage, err)
	}
	if runner.released != 0 {
		t.Fatalf("released count = %d, want 0 for an empty net diff", runner.released)
	}
}
