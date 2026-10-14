package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
)

type stubAgentAuthGate struct {
	statusState web.AgentAuthState
	statusErr   error
	statusCalls int
	startState  web.AgentAuthState
	startErr    error
	startCalls  int
}

func (g *stubAgentAuthGate) Status(context.Context) (web.AgentAuthState, error) {
	g.statusCalls++
	return g.statusState, g.statusErr
}

func (g *stubAgentAuthGate) StartDeviceAuth(context.Context) (web.AgentAuthState, error) {
	g.startCalls++
	if g.startState.State == "" {
		g.startState = g.statusState
	}
	return g.startState, g.startErr
}

func (g *stubAgentAuthGate) Verify(context.Context) (web.AgentAuthState, error) {
	return g.statusState, nil
}

func (g *stubAgentAuthGate) Configure(context.Context, string) (web.AgentAuthState, error) {
	return g.statusState, nil
}

func TestStringListFlagSetAndString(t *testing.T) {
	t.Parallel()

	var values stringListFlag
	if err := values.Set("a.json"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := values.Set("b.json"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got, want := values.String(), "a.json,b.json"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestWriteStdoutAndStderrLineCaptureToSink(t *testing.T) {
	t.Parallel()

	sink := &recordingTerminalLogSink{}
	var out strings.Builder
	logger := newTerminalLogger(&out, false)
	logger.sink = sink

	writeStdoutLine(logger, "  status=ok  ")
	writeStderrLine(logger, "  error: boom  ")
	writeStdoutLine(logger, "   ")
	writeStderrLine(logger, "")

	if got, want := len(sink.lines), 2; got != want {
		t.Fatalf("len(sink.lines) = %d, want %d (%v)", got, want, sink.lines)
	}
	if got, want := sink.lines[0], "status=ok"; got != want {
		t.Fatalf("sink.lines[0] = %q, want %q", got, want)
	}
	if got, want := sink.lines[1], "error: boom"; got != want {
		t.Fatalf("sink.lines[1] = %q, want %q", got, want)
	}
}

func TestRunResultPRHelpers(t *testing.T) {
	t.Parallel()

	withTopLevelPR := app.Result{PRURL: " https://github.com/acme/repo/pull/1 "}
	if !resultHasPR(withTopLevelPR) {
		t.Fatal("resultHasPR(top-level PR) = false, want true")
	}
	withRepoPR := app.Result{RepoResults: []app.RepoResult{{PRURL: "https://github.com/acme/repo/pull/2"}}}
	if !resultHasPR(withRepoPR) {
		t.Fatal("resultHasPR(repo PR) = false, want true")
	}
	if resultHasPR(app.Result{}) {
		t.Fatal("resultHasPR(empty) = true, want false")
	}
	noChanges := app.Result{
		NoChanges: true,
		RepoResults: []app.RepoResult{
			{Changed: false, PRURL: "https://github.com/acme/repo/pull/1"},
			{Changed: true, PRURL: "https://github.com/acme/repo/pull/2"},
		},
	}
	if got, want := completedPRURLs(noChanges), "https://github.com/acme/repo/pull/1,https://github.com/acme/repo/pull/2"; got != want {
		t.Fatalf("completedPRURLs(no changes) = %q, want %q", got, want)
	}
	changed := noChanges
	changed.NoChanges = false
	if got, want := completedPRURLs(changed), "https://github.com/acme/repo/pull/2"; got != want {
		t.Fatalf("completedPRURLs(changed only) = %q, want %q", got, want)
	}
}

func TestValidateRunConfigPromptImages(t *testing.T) {
	t.Parallel()

	if err := validateRunConfigPromptImages(config.Config{}); err != nil {
		t.Fatalf("validateRunConfigPromptImages(no images) error = %v", err)
	}
	if err := validateRunConfigPromptImages(config.Config{
		AgentHarness: agentruntime.HarnessCodex,
		Images:       []config.PromptImage{{Name: "shot.png"}},
	}); err != nil {
		t.Fatalf("validateRunConfigPromptImages(codex image) error = %v", err)
	}
	if err := validateRunConfigPromptImages(config.Config{
		AgentHarness: "unknown",
		Images:       []config.PromptImage{{Name: "shot.png"}},
	}); err == nil {
		t.Fatal("validateRunConfigPromptImages(unknown harness) error = nil, want non-nil")
	}
	if err := validateRunConfigPromptImages(config.Config{
		AgentHarness: agentruntime.HarnessClaude,
		Images:       []config.PromptImage{{Name: "shot.png"}},
	}); err != nil {
		t.Fatalf("validateRunConfigPromptImages(claude image) error = %v", err)
	}
}

func TestMaybeStartAgentAuthStartsClaudeLoginWhenBrowserAuthIsNeeded(t *testing.T) {
	t.Parallel()

	gate := &stubAgentAuthGate{
		statusState: web.AgentAuthState{
			Required: true,
			Ready:    false,
			State:    "needs_browser_login",
			Message:  "Claude login required",
		},
		startState: web.AgentAuthState{
			Required: true,
			Ready:    false,
			State:    "pending_browser_login",
		},
	}

	var logs []string
	maybeStartAgentAuth(
		context.Background(),
		agentruntime.Runtime{Harness: agentruntime.HarnessClaude, Command: "claude"},
		gate,
		func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	)

	if got, want := gate.startCalls, 1; got != want {
		t.Fatalf("startCalls = %d, want %d", got, want)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "status=start harness=claude action=start_device_auth state=pending_browser_login") {
		t.Fatalf("logs missing start marker: %q", got)
	}
}

func TestMaybeStartAgentAuthChecksStatusForNonClaudeBeforeUI(t *testing.T) {
	t.Parallel()

	gate := &stubAgentAuthGate{
		statusState: web.AgentAuthState{
			Required: true,
			Ready:    true,
			State:    "ready",
		},
	}

	maybeStartAgentAuth(
		context.Background(),
		agentruntime.Runtime{Harness: agentruntime.HarnessCodex, Command: "codex"},
		gate,
		func(string, ...any) {},
	)

	if got, want := gate.statusCalls, 1; got != want {
		t.Fatalf("statusCalls = %d, want %d", got, want)
	}
	if got, want := gate.startCalls, 0; got != want {
		t.Fatalf("startCalls = %d, want %d", got, want)
	}
}

func TestMaybeStartAgentAuthStartsWhenClaudeCredentialsNeedConfigure(t *testing.T) {
	t.Parallel()

	gate := &stubAgentAuthGate{
		statusState: web.AgentAuthState{
			Required:         true,
			Ready:            false,
			State:            "needs_configure",
			ConfigureCommand: claudeCredentialsConfigureCommand,
		},
		startState: web.AgentAuthState{
			Required: true,
			Ready:    false,
			State:    "pending_browser_login",
		},
	}

	maybeStartAgentAuth(
		context.Background(),
		agentruntime.Runtime{Harness: agentruntime.HarnessClaude, Command: "claude"},
		gate,
		func(string, ...any) {},
	)

	if got, want := gate.startCalls, 1; got != want {
		t.Fatalf("startCalls = %d, want %d", got, want)
	}
}

func TestMaybeStartAgentAuthSkipsWhenClaudeNeedsGitHubTokenConfigure(t *testing.T) {
	t.Parallel()

	gate := &stubAgentAuthGate{
		statusState: web.AgentAuthState{
			Required:         true,
			Ready:            false,
			State:            "needs_configure",
			ConfigureCommand: claudeGitHubConfigureCommand,
		},
	}

	maybeStartAgentAuth(
		context.Background(),
		agentruntime.Runtime{Harness: agentruntime.HarnessClaude, Command: "claude"},
		gate,
		func(string, ...any) {},
	)

	if got, want := gate.startCalls, 0; got != want {
		t.Fatalf("startCalls = %d, want %d", got, want)
	}
}

func TestMaybeStartAgentAuthLogsStartErrors(t *testing.T) {
	t.Parallel()

	gate := &stubAgentAuthGate{
		statusState: web.AgentAuthState{
			Required: true,
			Ready:    false,
			State:    "unexpected_auth_state",
			Message:  "Claude login required",
		},
		startState: web.AgentAuthState{
			Required: true,
			Ready:    false,
			State:    "error",
			Message:  "start claude auth login: exit status 2",
		},
		startErr: errors.New("exit status 2"),
	}

	var logs []string
	maybeStartAgentAuth(
		context.Background(),
		agentruntime.Runtime{Harness: agentruntime.HarnessClaude, Command: "claude"},
		gate,
		func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	)

	if got, want := gate.startCalls, 1; got != want {
		t.Fatalf("startCalls = %d, want %d", got, want)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, `status=warn harness=claude action=start_device_auth err="exit status 2" detail="start claude auth login: exit status 2"`) {
		t.Fatalf("logs missing detailed start failure: %q", got)
	}
}

func TestShouldEnableAgentAuthConfigure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		harness string
		want    bool
	}{
		{name: "codex", harness: agentruntime.HarnessCodex, want: true},
		{name: "claude", harness: agentruntime.HarnessClaude, want: true},
		{name: "mixed-case-codex", harness: "  CoDeX  ", want: true},
		{name: "unknown", harness: "custom", want: false},
		{name: "empty", harness: "", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldEnableAgentAuthConfigure(tt.harness); got != tt.want {
				t.Fatalf("shouldEnableAgentAuthConfigure(%q) = %v, want %v", tt.harness, got, tt.want)
			}
		})
	}
}

func TestJoinPRURLsAndCountChangedRepos(t *testing.T) {
	t.Parallel()

	results := []app.RepoResult{
		{Changed: true, PRURL: " https://github.com/acme/repo-a/pull/1 "},
		{Changed: false, PRURL: "https://github.com/acme/repo-b/pull/2"},
		{Changed: true, PRURL: ""},
		{Changed: true, PRURL: "https://github.com/acme/repo-c/pull/3"},
	}
	if got, want := joinPRURLs(results), "https://github.com/acme/repo-a/pull/1,https://github.com/acme/repo-c/pull/3"; got != want {
		t.Fatalf("joinPRURLs() = %q, want %q", got, want)
	}
	if got, want := joinAllPRURLs(results), "https://github.com/acme/repo-a/pull/1,https://github.com/acme/repo-b/pull/2,https://github.com/acme/repo-c/pull/3"; got != want {
		t.Fatalf("joinAllPRURLs() = %q, want %q", got, want)
	}
	if got, want := countChangedRepos(results), 3; got != want {
		t.Fatalf("countChangedRepos() = %d, want %d", got, want)
	}
}

func TestMarshalRunConfigJSONReturnsJSONPayload(t *testing.T) {
	t.Parallel()

	payload, ok := marshalRunConfigJSON(config.Config{
		RepoURL: "git@github.com:acme/repo.git",
		Prompt:  "fix tests",
	})
	if !ok {
		t.Fatal("marshalRunConfigJSON() ok = false, want true")
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got, want := decoded["repoUrl"], "git@github.com:acme/repo.git"; got != want {
		t.Fatalf("repoUrl = %v, want %q", got, want)
	}
}

func TestMarshalRunConfigJSONStoresLibraryTaskForReplay(t *testing.T) {
	t.Parallel()

	payload, ok := marshalRunConfigJSON(config.Config{
		RepoURL:         "git@github.com:acme/repo.git",
		BaseBranch:      "release",
		LibraryTaskName: "unit-test-coverage",
		Prompt:          "expanded library prompt",
	})
	if !ok {
		t.Fatal("marshalRunConfigJSON() ok = false, want true")
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got, want := decoded["libraryTaskName"], "unit-test-coverage"; got != want {
		t.Fatalf("libraryTaskName = %v, want %q", got, want)
	}
	if got := strings.TrimSpace(fmt.Sprint(decoded["prompt"])); got != "" {
		t.Fatalf("prompt = %q, want empty for library replay payload", got)
	}

	cfg, err := localruntime.ParseRunConfigJSON(payload)
	if err != nil {
		t.Fatalf("ParseRunConfigJSON() error = %v", err)
	}
	if got, want := cfg.LibraryTaskName, "unit-test-coverage"; got != want {
		t.Fatalf("LibraryTaskName = %q, want %q", got, want)
	}
	if got := cfg.Prompt; !strings.Contains(got, "100% unit-test coverage") {
		t.Fatalf("Prompt = %q, want expanded library prompt", got)
	}
}

func TestFailureFollowUpPromptDefaultWhenNoPaths(t *testing.T) {
	t.Parallel()

	got := failureFollowUpPrompt(nil, app.Result{}, config.Config{})
	if !strings.Contains(got, failureFollowUpRequiredPrompt) {
		t.Fatalf("prompt missing required instructions: %q", got)
	}
	if !strings.Contains(got, ".log/local/<request timestamp>/<request sequence>") {
		t.Fatalf("prompt missing default log dir hint: %q", got)
	}
	if !strings.Contains(got, ".log/local/<request timestamp>/<request sequence>/term") {
		t.Fatalf("prompt missing default legacy log file hint: %q", got)
	}
	if !strings.Contains(got, ".log/local/<request timestamp>/<request sequence>/terminal.log") {
		t.Fatalf("prompt missing default log path hint: %q", got)
	}

	if !strings.Contains(got, "When failures occur, send a response back to the calling agent that clearly states failure and includes the error details. Use explicit `Failure:` and `Error details:` fields.") {
		t.Fatalf("prompt missing failure response instruction: %q", got)
	}
	if !strings.Contains(got, "If a repository is not initialized after clone, use only gh CLI/git tools to create and push a main branch, then continue once git state is ready for work.") {
		t.Fatalf("prompt missing uninitialized-repo instruction: %q", got)
	}
	if !strings.Contains(got, "Do not commit, push, create or update pull requests, or watch remote CI/CD from inside this agent runtime.") {
		t.Fatalf("prompt missing remote operations handoff: %q", got)
	}
	if !strings.Contains(got, `"repos":["git@github.com:Molten-Bot/agent_00.git"],"targetSubdir":".","prompt":"`+failureFollowUpRequiredPrompt+`"`) {
		t.Fatalf("prompt missing follow-up payload shape: %q", got)
	}
	if !strings.Contains(got, "Treat the original task prompt as failure context only; do not implement that requested product change here unless it is required to fix agent_00 failure handling.") {
		t.Fatalf("prompt missing failure follow-up scope boundary: %q", got)
	}
	if !strings.Contains(got, "Only return a no-op when the task is genuinely review/investigation-only") {
		t.Fatalf("prompt missing no-op completion carve-out: %q", got)
	}
}

func TestFailureFollowUpPromptIncludesFailureContext(t *testing.T) {
	t.Parallel()

	got := failureFollowUpPrompt(
		[]string{"/workspace/.log/local/1775600653/000013/terminal.log"},
		app.Result{
			ExitCode:     app.ExitClone,
			Err:          errors.New("clone: repository not found"),
			WorkspaceDir: "/tmp/run-123",
			Branch:       "moltenhub-fix-clone",
			PRURL:        "https://github.com/acme/repo/pull/17",
		},
		config.Config{
			Repos: []string{
				"git@github.com:acme/repo-a.git",
				"git@github.com:acme/repo-b.git",
			},
		},
	)

	for _, want := range []string{
		"Observed failure context:",
		"- exit_code=40",
		`- error="clone: repository not found"`,
		"- workspace_dir=/tmp/run-123",
		"- branch=moltenhub-fix-clone",
		"- pr_url=https://github.com/acme/repo/pull/17",
		"- repos=git@github.com:acme/repo-a.git,git@github.com:acme/repo-b.git",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q: %q", want, got)
		}
	}
}

func TestShouldQueueUnexpectedNoChangesFollowUpRequiresMissingPR(t *testing.T) {
	t.Parallel()

	runCfg := config.Config{Prompt: "fix broken task handling"}

	ok, reason := shouldQueueUnexpectedNoChangesFollowUp(app.Result{NoChanges: true}, runCfg)
	if !ok || reason != "" {
		t.Fatalf("shouldQueueUnexpectedNoChangesFollowUp(no PR) = (%v, %q), want (true, \"\")", ok, reason)
	}

	ok, reason = shouldQueueUnexpectedNoChangesFollowUp(app.Result{
		NoChanges: true,
		PRURL:     "https://github.com/acme/repo/pull/1",
	}, runCfg)
	if ok {
		t.Fatal("shouldQueueUnexpectedNoChangesFollowUp(existing PR) = true, want false")
	}
	if reason != "task already has a pull request" {
		t.Fatalf("reason = %q, want %q", reason, "task already has a pull request")
	}
}

func TestShouldQueueUnexpectedNoChangesFollowUpSkipsVerificationPrompt(t *testing.T) {
	t.Parallel()

	runCfg := config.Config{
		Prompt: "Make sure that the openapi.yml is up to date with any new changes in the api.\n\nFor implementation or repository-change requests, do not stop at analysis. Produce the smallest correct repository diff unless concrete file evidence proves no file changes are required.",
	}

	ok, reason := shouldQueueUnexpectedNoChangesFollowUp(app.Result{NoChanges: true}, runCfg)
	if ok {
		t.Fatal("shouldQueueUnexpectedNoChangesFollowUp(up-to-date verification) = true, want false")
	}
	if reason != "original prompt does not clearly require repository changes" {
		t.Fatalf("reason = %q, want %q", reason, "original prompt does not clearly require repository changes")
	}
}

func TestLocalNoChangesFailureResultForActionablePrompt(t *testing.T) {
	t.Parallel()

	result, failed := localNoChangesFailureResult(localSubmitSource, app.Result{
		NoChanges:    true,
		WorkspaceDir: "/tmp/work",
		Branch:       "moltenhub-build-release",
	}, config.Config{Prompt: "fix broken release generation"})
	if !failed {
		t.Fatal("localNoChangesFailureResult(actionable) failed = false, want true")
	}
	if result.Err == nil {
		t.Fatal("localNoChangesFailureResult(actionable) Err = nil, want error")
	}
	if result.ExitCode != app.ExitCodex {
		t.Fatalf("ExitCode = %d, want %d", result.ExitCode, app.ExitCodex)
	}
	if !result.NoChanges {
		t.Fatal("NoChanges = false, want true")
	}
	if !strings.Contains(result.Err.Error(), "no file changes and no pull request") {
		t.Fatalf("Err = %v, want no-change failure detail", result.Err)
	}
	for _, want := range []string{
		"Root causes:",
		"no tracked file changes",
		"no pull request URL",
		"prompt requested repository changes",
		"no concrete repository evidence justified a no-op",
	} {
		if !strings.Contains(result.Err.Error(), want) {
			t.Fatalf("Err = %v, want root-cause detail %q", result.Err, want)
		}
	}
}

func TestLocalNoChangesFailureResultForGeneratedFollowUpPrompt(t *testing.T) {
	t.Parallel()

	prompt := unexpectedNoChangesFollowUpPrompt(nil, "local-1712345678-000001", app.Result{
		NoChanges: true,
	}, config.Config{
		Prompt: "fix release generation",
	})
	_, failed := localNoChangesFailureResult(noChangesFollowUpSource, app.Result{
		NoChanges: true,
	}, config.Config{Prompt: prompt})
	if !failed {
		t.Fatal("localNoChangesFailureResult(generated follow-up prompt) failed = false, want true")
	}
}

func TestLocalNoChangesFailureResultSkipsAllowedNoOps(t *testing.T) {
	t.Parallel()

	readOnly := config.Config{Prompt: "review logs and explain what happened"}
	if _, failed := localNoChangesFailureResult(localSubmitSource, app.Result{NoChanges: true}, readOnly); failed {
		t.Fatal("localNoChangesFailureResult(read-only) failed = true, want false")
	}

	conditionalNoOp := config.Config{Prompt: "Build the June 30 release. If there are no changes, do not produce an output change in the JSON."}
	if _, failed := localNoChangesFailureResult(localSubmitSource, app.Result{NoChanges: true}, conditionalNoOp); failed {
		t.Fatal("localNoChangesFailureResult(conditional no-op) failed = true, want false")
	}

	verified := app.Result{NoChanges: true, NoChangeEvidence: true}
	if _, failed := localNoChangesFailureResult(localSubmitSource, verified, config.Config{Prompt: "fix broken release generation"}); failed {
		t.Fatal("localNoChangesFailureResult(concrete evidence) failed = true, want false")
	}

	withPR := app.Result{NoChanges: true, PRURL: "https://github.com/acme/repo/pull/1"}
	if _, failed := localNoChangesFailureResult(localSubmitSource, withPR, config.Config{Prompt: "fix broken release generation"}); failed {
		t.Fatal("localNoChangesFailureResult(existing PR) failed = true, want false")
	}
}

func TestRunSingleActionableNoChangesReturnsFailure(t *testing.T) {
	configPath := writeRunSingleConfig(t, "fix broken release generation")
	previous := runSingleHarness
	t.Cleanup(func() { runSingleHarness = previous })

	runSingleHarness = func(_ context.Context, cfg config.Config, _ func(string, ...any)) app.Result {
		if cfg.Prompt != "fix broken release generation" {
			t.Fatalf("Prompt = %q", cfg.Prompt)
		}
		return app.Result{
			ExitCode:     app.ExitSuccess,
			WorkspaceDir: "/tmp/work",
			Branch:       "moltenhub-fix",
			NoChanges:    true,
		}
	}

	if code := runSingle([]string{"--config", configPath}); code != app.ExitCodex {
		t.Fatalf("runSingle() = %d, want %d", code, app.ExitCodex)
	}
}

func TestRunSingleFailurePrintsExplicitFailureFields(t *testing.T) {
	configPath := writeRunSingleConfig(t, "fix broken release generation")
	previousHarness := runSingleHarness
	t.Cleanup(func() { runSingleHarness = previousHarness })

	runSingleHarness = func(_ context.Context, _ config.Config, _ func(string, ...any)) app.Result {
		return app.Result{
			ExitCode: app.ExitCodex,
			Err:      errors.New("codex: process exited with status 1"),
		}
	}

	previousStderr := os.Stderr
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = writePipe
	t.Cleanup(func() {
		os.Stderr = previousStderr
		_ = readPipe.Close()
		_ = writePipe.Close()
	})

	if code := runSingle([]string{"--config", configPath}); code != app.ExitCodex {
		t.Fatalf("runSingle() = %d, want %d", code, app.ExitCodex)
	}
	_ = writePipe.Close()
	outputBytes, err := io.ReadAll(readPipe)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	output := string(outputBytes)
	if !strings.Contains(output, "Failure: task failed.") {
		t.Fatalf("stderr = %q, want Failure field", output)
	}
	if !strings.Contains(output, "Error details: codex: process exited with status 1") {
		t.Fatalf("stderr = %q, want Error details field", output)
	}
	if strings.Contains(output, "error: codex: process exited with status 1") {
		t.Fatalf("stderr = %q, want no legacy error prefix", output)
	}
}

func TestRunSingleConcreteNoChangeEvidenceStaysSuccess(t *testing.T) {
	configPath := writeRunSingleConfig(t, "fix broken release generation")
	previous := runSingleHarness
	t.Cleanup(func() { runSingleHarness = previous })

	runSingleHarness = func(_ context.Context, _ config.Config, _ func(string, ...any)) app.Result {
		return app.Result{
			ExitCode:         app.ExitSuccess,
			WorkspaceDir:     "/tmp/work",
			Branch:           "moltenhub-fix",
			NoChanges:        true,
			NoChangeEvidence: true,
		}
	}

	if code := runSingle([]string{"--config", configPath}); code != app.ExitSuccess {
		t.Fatalf("runSingle() = %d, want %d", code, app.ExitSuccess)
	}
}

func writeRunSingleConfig(t *testing.T, prompt string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"repo":"git@github.com:acme/repo.git","prompt":%q}`, prompt)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestShouldQueueUnexpectedNoChangesFollowUpQueuesDespiteConcreteNoOpEvidenceLog(t *testing.T) {
	t.Parallel()

	logPath := writeNoChangeEvidenceLog(t)
	runCfg := config.Config{
		Prompt: "Move all CSS that lives in page files into the app's global CSS file.",
	}

	ok, reason := shouldQueueUnexpectedNoChangesFollowUpWithLogs(app.Result{NoChanges: true}, runCfg, []string{logPath})
	if !ok || reason != "" {
		t.Fatalf("shouldQueueUnexpectedNoChangesFollowUpWithLogs(concrete evidence) = (%v, %q), want (true, \"\")", ok, reason)
	}
}

func TestShouldQueueUnexpectedNoChangesFollowUpIgnoresNoOpEvidenceFromStderr(t *testing.T) {
	t.Parallel()

	logPath := writeNoChangeEvidenceLogWithStream(t, "stderr")
	runCfg := config.Config{
		Prompt: "Move all CSS that lives in page files into the app's global CSS file.",
	}

	ok, reason := shouldQueueUnexpectedNoChangesFollowUpWithLogs(app.Result{NoChanges: true}, runCfg, []string{logPath})
	if !ok || reason != "" {
		t.Fatalf("shouldQueueUnexpectedNoChangesFollowUpWithLogs(stderr evidence) = (%v, %q), want (true, \"\")", ok, reason)
	}
}

func TestShouldQueueFailureFollowUpSkipsNestedFailureFollowUpSource(t *testing.T) {
	t.Parallel()

	ok, reason := shouldQueueFailureFollowUp(failureFollowUpSource, app.Result{Err: errors.New("still failing")})
	if ok {
		t.Fatal("shouldQueueFailureFollowUp(failure_followup) = true, want false")
	}
	if reason != "run is already a failure follow-up" {
		t.Fatalf("reason = %q, want %q", reason, "run is already a failure follow-up")
	}

	ok, reason = shouldQueueFailureFollowUp(noChangesEscalationSource, app.Result{Err: errors.New("still failing")})
	if ok {
		t.Fatal("shouldQueueFailureFollowUp(no_changes_escalation) = true, want false")
	}
	if reason != "run is already a failure follow-up" {
		t.Fatalf("reason = %q, want %q", reason, "run is already a failure follow-up")
	}

	ok, reason = shouldQueueFailureFollowUp(localSubmitSource, app.Result{Err: errors.New("clone failed")})
	if !ok || reason != "" {
		t.Fatalf("shouldQueueFailureFollowUp(local_submit,error) = (%v, %q), want (true, \"\")", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp(localSubmitSource, app.Result{})
	if ok {
		t.Fatal("shouldQueueFailureFollowUp(local_submit,nil error) = true, want false")
	}
	if reason != "failed task did not include an error" {
		t.Fatalf("reason = %q, want %q", reason, "failed task did not include an error")
	}
}

func TestShouldQueueFailureRerunSkipsNestedFailureRerunSource(t *testing.T) {
	t.Parallel()

	ok, reason := shouldQueueFailureRerun(rerunSource, app.Result{Err: errors.New("still failing")})
	if ok {
		t.Fatal("shouldQueueFailureRerun(rerun) = true, want false")
	}
	if reason != "run is already a failure rerun" {
		t.Fatalf("reason = %q, want %q", reason, "run is already a failure rerun")
	}

	ok, reason = shouldQueueFailureRerun(localSubmitSource, app.Result{Err: errors.New("clone failed")})
	if ok || reason != automaticFailureRerunDisabledReason {
		t.Fatalf("shouldQueueFailureRerun(local_submit,error) = (%v, %q), want (false, %q)", ok, reason, automaticFailureRerunDisabledReason)
	}

	ok, reason = shouldQueueFailureRerun(failureFollowUpSource, app.Result{Err: errors.New("clone failed")})
	if ok {
		t.Fatal("shouldQueueFailureRerun(failure_followup,error) = true, want false")
	}
	if reason != "run is already a failure follow-up" {
		t.Fatalf("reason = %q, want %q", reason, "run is already a failure follow-up")
	}

	ok, reason = shouldQueueFailureRerun(noChangesEscalationSource, app.Result{Err: errors.New("clone failed")})
	if ok {
		t.Fatal("shouldQueueFailureRerun(no_changes_escalation,error) = true, want false")
	}
	if reason != "run is already a failure follow-up" {
		t.Fatalf("reason = %q, want %q", reason, "run is already a failure follow-up")
	}

	ok, reason = shouldQueueFailureRerun(localSubmitSource, app.Result{})
	if ok {
		t.Fatal("shouldQueueFailureRerun(local_submit,nil error) = true, want false")
	}
	if reason != "failed task did not include an error" {
		t.Fatalf("reason = %q, want %q", reason, "failed task did not include an error")
	}
}

func TestEnqueueFailureRerunBypassesDuplicateSuppression(t *testing.T) {
	t.Parallel()

	runCfg := config.Config{
		Repo:   "git@github.com:acme/repo.git",
		Prompt: "fix failing checks",
	}

	var (
		gotCfg                  config.Config
		gotAllowFailureFollowUp bool
		gotSource               string
		gotForce                bool
	)

	requestID, err := enqueueFailureRerun(context.Background(), func(_ context.Context, cfg config.Config, allowFailureFollowUp bool, source string, force bool) (string, error) {
		gotCfg = cfg
		gotAllowFailureFollowUp = allowFailureFollowUp
		gotSource = source
		gotForce = force
		return "local-rerun-1", nil
	}, runCfg)
	if err != nil {
		t.Fatalf("enqueueFailureRerun() error = %v", err)
	}
	if requestID != "local-rerun-1" {
		t.Fatalf("requestID = %q, want %q", requestID, "local-rerun-1")
	}
	if !reflect.DeepEqual(gotCfg, runCfg) {
		t.Fatalf("runCfg = %#v, want %#v", gotCfg, runCfg)
	}
	if gotAllowFailureFollowUp {
		t.Fatal("allowFailureFollowUp = true, want false")
	}
	if gotSource != rerunSource {
		t.Fatalf("source = %q, want %q", gotSource, rerunSource)
	}
	if !gotForce {
		t.Fatal("force = false, want true")
	}
}

func TestShouldEscalateNoChangesFollowUpRequiresFollowUpSourceAndMissingPR(t *testing.T) {
	t.Parallel()

	runCfg := config.Config{Prompt: "change the website to pink"}

	ok, reason := shouldEscalateNoChangesFollowUp(localSubmitSource, app.Result{NoChanges: true}, runCfg)
	if ok {
		t.Fatal("shouldEscalateNoChangesFollowUp(local_submit) = true, want false")
	}
	if reason != "run is not a no-changes follow-up" {
		t.Fatalf("reason = %q, want %q", reason, "run is not a no-changes follow-up")
	}

	ok, reason = shouldEscalateNoChangesFollowUp(noChangesFollowUpSource, app.Result{NoChanges: true}, runCfg)
	if !ok || reason != "" {
		t.Fatalf("shouldEscalateNoChangesFollowUp(no_changes_followup,actionable) = (%v, %q), want (true, \"\")", ok, reason)
	}

	ok, reason = shouldEscalateNoChangesFollowUp(noChangesFollowUpSource, app.Result{
		NoChanges: true,
		PRURL:     "https://github.com/acme/repo/pull/1",
	}, runCfg)
	if ok {
		t.Fatal("shouldEscalateNoChangesFollowUp(existing PR) = true, want false")
	}
	if reason != "task already has a pull request" {
		t.Fatalf("reason = %q, want %q", reason, "task already has a pull request")
	}

	ok, reason = shouldEscalateNoChangesFollowUp(noChangesFollowUpSource, app.Result{NoChanges: true}, config.Config{
		Prompt: "review the latest logs and explain what happened",
	})
	if ok {
		t.Fatal("shouldEscalateNoChangesFollowUp(read-only prompt) = true, want false")
	}
	if reason != "original prompt does not clearly request repository changes" {
		t.Fatalf("reason = %q, want %q", reason, "original prompt does not clearly request repository changes")
	}
}

func TestShouldEscalateNoChangesFollowUpQueuesDespiteConcreteNoOpEvidenceLog(t *testing.T) {
	t.Parallel()

	logPath := writeNoChangeEvidenceLog(t)
	runCfg := config.Config{Prompt: "fix the broken no-change handler"}

	ok, reason := shouldEscalateNoChangesFollowUpWithLogs(noChangesFollowUpSource, app.Result{NoChanges: true}, runCfg, []string{logPath})
	if !ok || reason != "" {
		t.Fatalf("shouldEscalateNoChangesFollowUpWithLogs(concrete evidence) = (%v, %q), want (true, \"\")", ok, reason)
	}
}

func TestUnexpectedNoChangesFollowUpRunConfigTargetsHarnessAndAddsContext(t *testing.T) {
	t.Parallel()

	logRoot := filepath.Join(t.TempDir(), ".log")
	logDir := filepath.Join(logRoot, "local", "1712345678", "000001")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	logPath := filepath.Join(logDir, logFileName)
	if err := os.WriteFile(logPath, []byte("dispatch status=no_changes\n"), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	runCfg := config.Config{
		Repos:        []string{"git@github.com:acme/repo.git"},
		BaseBranch:   "release/2026.04-hotfix",
		TargetSubdir: "cmd/harness",
		Prompt:       "fix the broken local no changes task handling",
	}
	result := app.Result{
		NoChanges:    true,
		WorkspaceDir: "/tmp/run-123",
		Branch:       "release/2026.04-hotfix",
	}

	cfg := unexpectedNoChangesFollowUpRunConfig("local-1712345678-000001", result, runCfg, logRoot)
	if got, want := cfg.BaseBranch, ""; got != want {
		t.Fatalf("BaseBranch = %q, want %q", got, want)
	}
	if got, want := cfg.TargetSubdir, "."; got != want {
		t.Fatalf("TargetSubdir = %q, want %q", got, want)
	}
	if got, want := cfg.Repos, []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Repos = %v, want %v", got, want)
	}

	expectedLogDir := filepath.Join(logRoot, followUpTaskLogArchiveSubdir, "local", "1712345678", "000001")

	for _, want := range []string{
		"Review the previous local task logs first.",
		"fix the underlying agent_00 application issue in this repository",
		"Treat the original task prompt as failure context only",
		"Only return a no-op if you can cite concrete repository evidence",
		expectedLogDir,
		filepath.Join(expectedLogDir, logFileName),
		"Observed no-change context:",
		"- request_id=local-1712345678-000001",
		"- workspace_dir=/tmp/run-123",
		"- branch=release/2026.04-hotfix",
		"- repos=git@github.com:acme/repo.git",
		"- target_subdir=cmd/harness",
		"Original task prompt:",
		"fix the broken local no changes task handling",
	} {
		if !strings.Contains(cfg.Prompt, want) {
			t.Fatalf("Prompt missing %q: %q", want, cfg.Prompt)
		}
	}
	if strings.Contains(cfg.Prompt, filepath.Join(logRoot, logFileName)) {
		t.Fatalf("Prompt should exclude aggregate terminal log path to avoid recursive follow-up reads: %q", cfg.Prompt)
	}
}

func TestUnexpectedNoChangesFollowUpRunConfigForcesRemoteDefaultBranch(t *testing.T) {
	t.Parallel()

	cfg := unexpectedNoChangesFollowUpRunConfig(
		"local-1712345678-000001",
		app.Result{
			NoChanges: true,
			Branch:    "moltenhub-add-the-emoji-picker-to-the-agent-profil",
		},
		config.Config{
			Repos:      []string{"git@github.com:acme/repo.git"},
			BaseBranch: "main",
		},
		t.TempDir(),
	)

	if got, want := cfg.BaseBranch, ""; got != want {
		t.Fatalf("BaseBranch = %q, want %q", got, want)
	}
}

func TestUnexpectedNoChangesFollowUpRunConfigUsesHarnessRepoAndKeepsOriginalReposInPrompt(t *testing.T) {
	t.Parallel()

	runCfg := config.Config{
		Repo: "git@github.com:acme/repo-a.git",
		Repos: []string{
			"git@github.com:acme/repo-b.git",
			"git@github.com:acme/repo-a.git",
		},
	}
	cfg := unexpectedNoChangesFollowUpRunConfig(
		"local-1712345678-000001",
		app.Result{NoChanges: true},
		runCfg,
		t.TempDir(),
	)

	if got, want := cfg.Repos, []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Repos = %v, want %v", got, want)
	}
	for _, want := range []string{
		"- repos=",
		"git@github.com:acme/repo-a.git",
		"git@github.com:acme/repo-b.git",
	} {
		if !strings.Contains(cfg.Prompt, want) {
			t.Fatalf("Prompt missing original repo context %q: %q", want, cfg.Prompt)
		}
	}
}

func TestUnexpectedNoChangesFollowUpRunConfigFallsBackToHarnessRepoWhenNoOriginalRepo(t *testing.T) {
	t.Parallel()

	cfg := unexpectedNoChangesFollowUpRunConfig(
		"local-1712345678-000001",
		app.Result{NoChanges: true},
		config.Config{},
		t.TempDir(),
	)

	if got, want := cfg.Repos, []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Repos = %v, want %v", got, want)
	}
}

func TestUnexpectedNoChangesFollowUpRunConfigUsesNoPathGuidanceWhenTaskLogsMissing(t *testing.T) {
	t.Parallel()

	logRoot := filepath.Join(t.TempDir(), ".log")
	runCfg := config.Config{
		Repos:        []string{"git@github.com:acme/repo.git"},
		BaseBranch:   "main",
		TargetSubdir: ".",
		Prompt:       "investigate missing changes",
	}
	result := app.Result{
		NoChanges: true,
		Branch:    "main",
	}

	cfg := unexpectedNoChangesFollowUpRunConfig("local-1712345678-000001", result, runCfg, logRoot)
	missingLogPath := filepath.Join(logRoot, "local", "1712345678", "000001")
	if strings.Contains(cfg.Prompt, missingLogPath) {
		t.Fatalf("Prompt should not include missing log path %q: %q", missingLogPath, cfg.Prompt)
	}
	if !strings.Contains(cfg.Prompt, "No local task log path was captured before the task completed without changes.") {
		t.Fatalf("Prompt missing no-path guidance: %q", cfg.Prompt)
	}
}

func TestEscalatedNoChangesFollowUpRunConfigAddsStricterPrompt(t *testing.T) {
	t.Parallel()

	logRoot := filepath.Join(t.TempDir(), ".log")
	logDir := filepath.Join(logRoot, "local", "1712345678", "000001")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	logPath := filepath.Join(logDir, logFileName)
	if err := os.WriteFile(logPath, []byte("dispatch status=no_changes\n"), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	runCfg := config.Config{
		Repos:        []string{"git@github.com:acme/repo.git"},
		BaseBranch:   "main",
		TargetSubdir: "internal/web",
		Prompt:       "change the website to pink",
	}

	cfg := escalatedNoChangesFollowUpRunConfig("local-1712345678-000001", app.Result{NoChanges: true}, runCfg, logRoot)
	if got, want := cfg.BaseBranch, ""; got != want {
		t.Fatalf("BaseBranch = %q, want %q", got, want)
	}
	if got, want := cfg.TargetSubdir, "."; got != want {
		t.Fatalf("TargetSubdir = %q, want %q", got, want)
	}
	if got, want := cfg.Repos, []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Repos = %v, want %v", got, want)
	}
	for _, want := range []string{
		"The original task and the no-changes follow-up both completed without file changes or a pull request.",
		"fix the underlying agent_00 application issue in this repository",
		"do not implement that requested product change here unless it is required to fix agent_00 failure handling",
		"Do not return another no-op unless you can cite exact file paths",
		"Observed repeated no-change context:",
		"- target_subdir=internal/web",
		"Original task prompt:",
		"change the website to pink",
	} {
		if !strings.Contains(cfg.Prompt, want) {
			t.Fatalf("Prompt missing %q: %q", want, cfg.Prompt)
		}
	}
}

func TestPromptRequestsRepositoryChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		prompt string
		want   bool
	}{
		{prompt: "change the website to pink", want: true},
		{prompt: "fix the broken auth flow", want: true},
		{prompt: "review the latest logs and explain what happened", want: false},
		{prompt: "review the latest logs and fix the broken auth flow", want: true},
		{prompt: "what changed in the container?", want: false},
		{prompt: "Make sure that the openapi.yml is up to date with any new changes in the api.", want: false},
		{prompt: "Confirm generated OpenAPI is up-to-date.", want: false},
		{prompt: "Ensure generated OpenAPI matches the API routes.", want: false},
		{prompt: "Fix: ensure generated OpenAPI matches the API routes.", want: true},
		{prompt: "Update generated OpenAPI to match the API routes.", want: true},
		{prompt: "Build the June 30 release. If there are no changes, do not produce an output change in the JSON.", want: false},
		{prompt: "Build the release. If no changes, don't create a pull request.", want: false},
		{prompt: "Validate the release. When nothing changed, leave unchanged and report the result.", want: false},
		{prompt: "If there are no changes, investigate and fix the collector.", want: true},
		{prompt: "If there are no changes, create an empty pull request.", want: true},
		{prompt: "", want: true},
	}

	for _, tt := range tests {
		if got := promptRequestsRepositoryChange(tt.prompt); got != tt.want {
			t.Fatalf("promptRequestsRepositoryChange(%q) = %v, want %v", tt.prompt, got, tt.want)
		}
	}
}

func writeNoChangeEvidenceLog(t *testing.T) string {
	t.Helper()

	return writeNoChangeEvidenceLogWithStream(t, "stdout")
}

func writeNoChangeEvidenceLogWithStream(t *testing.T, stream string) string {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), logFileName)
	lines := []string{
		"Done: no tracked diff. Requested CSS org already true.",
		"Page styles moved: none. [src/pages/index.astro](/tmp/repo/src/pages/index.astro:2) has no `<style>`, inline `style=`, CSS module, or route stylesheet.",
		"Only CSS import is existing global file: [src/styles/global.css](/tmp/repo/src/styles/global.css:1).",
	}
	var b strings.Builder
	for _, line := range lines {
		encoded := base64.StdEncoding.EncodeToString([]byte(line))
		fmt.Fprintf(&b, "dispatch request_id=req cmd phase=codex name=codex stream=%s b64=%s\n", stream, encoded)
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return logPath
}

func TestFollowUpTaskLogPathsArchivesLocalTaskLogs(t *testing.T) {
	t.Parallel()

	logRoot := filepath.Join(t.TempDir(), ".log")
	requestID := "local-1712345678-000001"
	sourceDir := filepath.Join(logRoot, "local", "1712345678", "000001")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}

	legacyContent := []byte("legacy log\n")
	if err := os.WriteFile(filepath.Join(sourceDir, legacyTaskLogFileName), legacyContent, 0o644); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
	currentContent := []byte("current log\n")
	if err := os.WriteFile(filepath.Join(sourceDir, logFileName), currentContent, 0o644); err != nil {
		t.Fatalf("write current log: %v", err)
	}

	paths := followUpTaskLogPaths(logRoot, requestID)
	archiveDir := filepath.Join(logRoot, followUpTaskLogArchiveSubdir, "local", "1712345678", "000001")
	wantPaths := []string{
		archiveDir,
		filepath.Join(archiveDir, legacyTaskLogFileName),
		filepath.Join(archiveDir, logFileName),
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("followUpTaskLogPaths() = %v, want %v", paths, wantPaths)
	}

	if got, err := os.ReadFile(filepath.Join(archiveDir, legacyTaskLogFileName)); err != nil || string(got) != string(legacyContent) {
		t.Fatalf("archived legacy log mismatch: content=%q err=%v", string(got), err)
	}
	if got, err := os.ReadFile(filepath.Join(archiveDir, logFileName)); err != nil || string(got) != string(currentContent) {
		t.Fatalf("archived current log mismatch: content=%q err=%v", string(got), err)
	}

	if err := os.RemoveAll(sourceDir); err != nil {
		t.Fatalf("remove source dir: %v", err)
	}
	if got := existingPaths(paths); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("existingPaths(archived) = %v, want %v", got, wantPaths)
	}
}

func TestShouldQueueFailureFollowUpQueuesNoDeltaFailures(t *testing.T) {
	t.Parallel()

	result := app.Result{
		Err: errors.New("task failed to meet completion requirements because this branch has no delta from `main`; No commits between main and moltenhub-fix"),
	}

	ok, reason := shouldQueueFailureFollowUp("local_submit", result)
	if !ok || reason != "" {
		t.Fatalf("shouldQueueFailureFollowUp() = (%v, %q), want (true, \"\")", ok, reason)
	}
}

func TestTaskLogDirAndTaskLogPathsValidateInputs(t *testing.T) {
	t.Parallel()

	if got, ok := taskLogDir("", "req-1"); ok || got != "" {
		t.Fatalf("taskLogDir(empty root) = (%q, %v), want (\"\", false)", got, ok)
	}
	if got, ok := taskLogDir("/tmp/.log", ""); ok || got != "" {
		t.Fatalf("taskLogDir(empty request) = (%q, %v), want (\"\", false)", got, ok)
	}
	if got := taskLogPaths("", "req-1"); got != nil {
		t.Fatalf("taskLogPaths(empty root) = %v, want nil", got)
	}

	paths := taskLogPaths("/tmp/.log", "local-1712345678-000001")
	want := []string{
		filepath.Join("/tmp/.log", "local", "1712345678", "000001"),
		filepath.Join("/tmp/.log", "local", "1712345678", "000001", legacyTaskLogFileName),
		filepath.Join("/tmp/.log", "local", "1712345678", "000001", logFileName),
	}
	if got, wantJoined := strings.Join(paths, "\n"), strings.Join(want, "\n"); got != wantJoined {
		t.Fatalf("taskLogPaths(...) = %q, want %q", got, wantJoined)
	}
}

func TestShouldQueueFailureFollowUpSkipsNonRemediableFailureReasons(t *testing.T) {
	t.Parallel()

	ok, reason := shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("codex: ERROR: Quota exceeded. Check your plan and billing details."),
	})
	if ok || !strings.Contains(reason, "quota exceeded") {
		t.Fatalf("shouldQueueFailureFollowUp(quota exceeded) = (%v, %q), want non-remediable quota skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("codex: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header"),
	})
	if ok || !strings.Contains(reason, "401 unauthorized") {
		t.Fatalf("shouldQueueFailureFollowUp(auth failure) = (%v, %q), want non-remediable auth skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("codex: codex reported failure: Failure: Live Claude task could not create PR. Error details: `Your organization has disabled Claude subscription access for Claude Code · Use an Anthropic API key instead, or ask your admin to enable access`"),
	})
	if ok || !strings.Contains(reason, "organization has disabled claude subscription access") {
		t.Fatalf("shouldQueueFailureFollowUp(Claude subscription disabled) = (%v, %q), want non-remediable subscription-access skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("git: verify remote write access for repo https://github.com/acme/repo.git branch \"moltenhub-fix\": exit status 128: remote: Write access to repository not granted. fatal: unable to access 'https://github.com/acme/repo.git/': The requested URL returned error: 403"),
	})
	if ok || !strings.Contains(reason, "write access to repository not granted") {
		t.Fatalf("shouldQueueFailureFollowUp(repo write access failure) = (%v, %q), want non-remediable repo access skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("clone: run git [clone --single-branch git@github.com:Molten-Bot/forum.git /workspace/repo]: exit status 128 (fatal: unable to access 'https://github.com/Molten-Bot/forum.git/': Could not resolve host: github.com)"),
	})
	if ok || !strings.Contains(reason, "could not resolve host") {
		t.Fatalf("shouldQueueFailureFollowUp(clone DNS failure) = (%v, %q), want non-remediable DNS skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("clone: run git [clone ...]: exit status 128"),
	})
	if !ok || reason != "" {
		t.Fatalf("shouldQueueFailureFollowUp(generic clone failure) = (%v, %q), want (true, \"\")", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("git: run git [push -u origin moltenhub-branch]: exit status 1: remote: refusing to allow an OAuth App to create or update workflow `.github/workflows/docker-release.yml` without `workflow` scope"),
	})
	if ok || !strings.Contains(reason, "refusing to allow an oauth app to create or update workflow") {
		t.Fatalf("shouldQueueFailureFollowUp(workflow scope failure) = (%v, %q), want non-remediable workflow-scope skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("codex: codex reported failure: Failure: user-portal changes not applied. Error details: sandbox rejected writes to `/home/developer/git/moltenbot/user-portal`: `writing outside of the project; rejected by user approval settings`."),
	})
	if ok || !strings.Contains(reason, "sandbox rejected writes to") {
		t.Fatalf("shouldQueueFailureFollowUp(sandbox write rejection) = (%v, %q), want non-remediable sandbox skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("codex: codex reported failure: Failure: Full site/assets download not completed. Error details: shell network blocked. `wget` failed DNS for `www.kaanawaveco.com`; `curl --resolve` failed connect to port 443. Browser tool crawled public pages, but cannot save binary assets."),
	})
	if ok || !strings.Contains(reason, "shell network blocked") {
		t.Fatalf("shouldQueueFailureFollowUp(shell network blocked) = (%v, %q), want non-remediable network skip", ok, reason)
	}

	ok, reason = shouldQueueFailureFollowUp("local_submit", app.Result{
		Err: errors.New("codex: codex reported failure: Failure: No June 14 release entry created. Error details: `git-changes-by-day` ran across all 10 repos after fetch. `2026-06-14` UTC rows: `0` in every repo. Raw `git log --all` confirmed same. `www` release schema rejects empty releases, so adding entry would require invented project/note content."),
	})
	if ok || !strings.Contains(reason, "would require invented") {
		t.Fatalf("shouldQueueFailureFollowUp(invented content) = (%v, %q), want non-remediable invented-content skip", ok, reason)
	}
}

func TestApplyDefaultAgentRuntimeConfig(t *testing.T) {
	t.Parallel()

	initCfg := localruntime.InitConfig{
		AgentHarness: "claude",
		AgentCommand: "claude-custom",
	}
	got := applyDefaultAgentRuntimeConfig(config.Config{}, initCfg)
	if got.AgentHarness != "claude" || got.AgentCommand != "claude-custom" {
		t.Fatalf("applyDefaultAgentRuntimeConfig() = %+v", got)
	}

	explicit := config.Config{AgentHarness: "codex", AgentCommand: "codex-custom"}
	got = applyDefaultAgentRuntimeConfig(explicit, initCfg)
	if got.AgentHarness != "codex" || got.AgentCommand != "codex-custom" {
		t.Fatalf("explicit run config should win; got %+v", got)
	}
}

func TestRunLocalDispatchReportsErrorState(t *testing.T) {
	t.Parallel()

	var logs []string
	outcome := runLocalDispatch(
		context.Background(),
		nil,
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		"code_for_me",
		"req-1",
		config.Config{
			RepoURL: "git@github.com:acme/repo.git",
			Prompt:  "fix tests",
		},
		0,
		nil,
	)

	if outcome.State != "error" {
		t.Fatalf("runLocalDispatch() state = %q, want error", outcome.State)
	}
	if outcome.Result.Err == nil {
		t.Fatal("runLocalDispatch() result error = nil, want non-nil")
	}
	assertLogContains(t, logs, "dispatch status=error request_id=req-1")
}

func TestTaskWorkspaceDirFromLogContent(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{
		"dispatch status=start request_id=req-1",
		"dispatch status=completed request_id=req-2 workspace=/tmp/ignore",
		"dispatch status=completed request_id=req-1 workspace=/tmp/work-old",
		"dispatch status=error request_id=req-1 workspace=/tmp/work-new err=\"boom\"",
	}, "\n")

	if got, want := taskWorkspaceDirFromLogContent(content, "req-1"), "/tmp/work-new"; got != want {
		t.Fatalf("taskWorkspaceDirFromLogContent() = %q, want %q", got, want)
	}
	if got := taskWorkspaceDirFromLogContent(content, "req-missing"); got != "" {
		t.Fatalf("taskWorkspaceDirFromLogContent(missing) = %q, want empty", got)
	}
}

func TestTaskWorkspaceDirFromLogDir(t *testing.T) {
	t.Parallel()

	logDir := t.TempDir()
	logContent := "dispatch status=completed request_id=req-log workspace=/tmp/workspace-from-log\n"
	if err := os.WriteFile(filepath.Join(logDir, logFileName), []byte(logContent), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	if got, want := taskWorkspaceDirFromLogDir(logDir, "req-log"), "/tmp/workspace-from-log"; got != want {
		t.Fatalf("taskWorkspaceDirFromLogDir() = %q, want %q", got, want)
	}
}

func TestRemoveManagedWorkspaceDir(t *testing.T) {
	t.Setenv("HARNESS_WORKSPACE_RAM_BASE", filepath.Join(t.TempDir(), "ram-base"))
	t.Setenv("HARNESS_WORKSPACE_DISK_BASE", filepath.Join(t.TempDir(), "disk-base"))
	t.Setenv("HARNESS_WORKSPACE_ROOT_NAME", "tasks-root")

	managedDir := filepath.Join(
		os.Getenv("HARNESS_WORKSPACE_RAM_BASE"),
		"tasks-root",
		"0123456789abcdef0123456789abcdef",
	)
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		t.Fatalf("mkdir managed dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write managed file: %v", err)
	}

	removed, err := removeManagedWorkspaceDir(managedDir)
	if err != nil {
		t.Fatalf("removeManagedWorkspaceDir(managed) error = %v", err)
	}
	if !removed {
		t.Fatal("removeManagedWorkspaceDir(managed) removed = false, want true")
	}
	if _, err := os.Stat(managedDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed dir still exists after cleanup: %v", err)
	}

	unmanagedDir := filepath.Join(t.TempDir(), "unmanaged")
	if err := os.MkdirAll(unmanagedDir, 0o755); err != nil {
		t.Fatalf("mkdir unmanaged dir: %v", err)
	}
	removed, err = removeManagedWorkspaceDir(unmanagedDir)
	if err != nil {
		t.Fatalf("removeManagedWorkspaceDir(unmanaged) error = %v", err)
	}
	if removed {
		t.Fatal("removeManagedWorkspaceDir(unmanaged) removed = true, want false")
	}
	if _, err := os.Stat(unmanagedDir); err != nil {
		t.Fatalf("unmanaged dir stat error = %v, want present", err)
	}
}
