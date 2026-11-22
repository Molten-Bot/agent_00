package app

import (
	"context"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
)

// markdownFailureReport mirrors the shape agents actually produce for the
// mandated Failure/Error details fields: a Markdown report that leads with its
// summary, so both fields sit well above the terminal output window.
func markdownFailureReport() string {
	return strings.Join([]string{
		"## Failure",
		"",
		"**Failure:** Cannot complete the requested change. Zero repository changes made.",
		"",
		"**Error details:**",
		"",
		"```",
		"EROFS: read-only file system, mkdir '/config/home/.claude/session-env'",
		"```",
		"",
		"Bash fails during session bootstrap, before any command runs.",
		"",
		"### Unblock path",
		"",
		"- make the configuration directory writable, or",
		"- point the configuration directory at a writable path.",
		"",
		"Nothing was changed.",
	}, "\n")
}

func TestCodexReportedFailureDetectsMarkdownFailureFields(t *testing.T) {
	t.Parallel()

	res := execx.Result{Stdout: markdownFailureReport()}
	failed, detail := codexReportedFailure(res)
	if !failed {
		t.Fatal("codexReportedFailure() = false, want true for a Markdown failure report")
	}
	if !strings.HasPrefix(detail, "Failure: Cannot complete the requested change.") {
		t.Fatalf("detail = %q, want the reported failure summary", detail)
	}

	_, full := codexFailureDetailWithErrorDetails(res, detail)
	if !strings.Contains(full, "read-only file system") {
		t.Fatalf("detail = %q, want the reported error details", full)
	}
	if strings.Contains(full, "```") {
		t.Fatalf("detail = %q, want Markdown code fences dropped", full)
	}
}

func TestCodexReportedFailureIgnoresFailureFieldWithoutErrorDetails(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"**Failure:** one validation command could not run.",
		"Retried and recovered.",
		"step 3",
		"step 4",
		"step 5",
		"step 6",
		"step 7",
		"step 8",
		"step 9",
		"step 10",
		"Done.",
	}, "\n")
	if failed, detail := codexReportedFailure(execx.Result{Stdout: output}); failed {
		t.Fatalf("codexReportedFailure() = true (%q), want false without reported error details", detail)
	}
}

func TestCodexReportedFailureDetectsBlockerAfterResolvedIssue(t *testing.T) {
	t.Parallel()

	res := execx.Result{Stdout: strings.Join([]string{
		"Resolved issue: Test cases shared temporary state.",
		"**Failure:** The full test suite still fails.",
		"**Error details:** A separate test times out waiting for a worker.",
	}, "\n")}

	failed, detail := codexReportedFailure(res)
	if !failed || detail != "Failure: The full test suite still fails." {
		t.Fatalf("codexReportedFailure() = (%v, %q), want the unresolved failure", failed, detail)
	}
	if _, full := codexFailureDetailWithErrorDetails(res, detail); !strings.Contains(full, "A separate test times out waiting for a worker.") {
		t.Fatalf("failure detail = %q, want the unresolved error details", full)
	}
}

// The terminal-window guard keeps a mid-run "Failure:" progress update from
// failing a run that went on to finish. Reading both mandated fields out of
// the report header must not reach around that guard.
func TestCodexReportedFailureIgnoresFailureFieldsBelowReportHeader(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"Reviewed the failing job and reproduced it locally.",
		"Patched the duplicated helper.",
		"Failure: focused tests failed on compile.",
		"Error details: duplicate helper declaration.",
		"apply patch",
		"patch: completed",
		"go build ./...",
		"go test ./internal/app",
		"ok  \tgithub.com/example/project/internal/app",
		"go test ./...",
		"ok  \tgithub.com/example/project/internal/web",
		"Refactor complete. Tests pass.",
	}, "\n")

	if failed, detail := codexReportedFailure(execx.Result{Stdout: output}); failed {
		t.Fatalf("codexReportedFailure() = true (%q), want false for a mid-run progress marker", detail)
	}
}

func TestCodexExtractErrorDetailDropsFencesWithInfoString(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"**Failure:** could not apply the migration.",
		"**Error details:**",
		"```json",
		`{"code":"EROFS","message":"read-only file system"}`,
		"```",
	}, "\n")

	detail := codexExtractErrorDetail(output)
	if !strings.Contains(detail, `"code":"EROFS"`) {
		t.Fatalf("codexExtractErrorDetail() = %q, want the fenced payload", detail)
	}
	if strings.Contains(detail, "```") {
		t.Fatalf("codexExtractErrorDetail() = %q, want Markdown code fences dropped", detail)
	}
}

func TestCodexReportedRolloutFailurePreservesInlineErrorDetailContinuations(t *testing.T) {
	t.Parallel()

	statusLines := "\nMCP flags default to off.\nWorker flags default to off."
	for _, tc := range []struct {
		name           string
		detailsHeading bool
		continuation   string
		wantReason     string
	}{
		{
			name:       "external prerequisites only",
			wantReason: failurefollowup.ExternalRolloutPrerequisitesReason,
		},
		{
			name:         "deployment tests fail",
			continuation: "\n\nDeployment tests fail.",
		},
		{
			name:           "deployment test failure under a Markdown subheading",
			detailsHeading: true,
			continuation:   "\n\n### Deployment tests\nThe readiness assertion failed.",
		},
		{
			name:         "deployment test failure after three status lines",
			continuation: statusLines + "\nDeployment tests fail.",
		},
		{
			name:         "byte limit reached before late test failure",
			continuation: "\nAPI flags default to off." + strings.Repeat(" ", 320) + "Deployment tests fail.",
		},
		{
			name:         "three status lines fit",
			continuation: statusLines,
			wantReason:   failurefollowup.ExternalRolloutPrerequisitesReason,
		},
		{
			name:         "resolved issue after three status lines",
			continuation: statusLines + "\n**Resolved issue:** Deployment tests failed before the fix.",
			wantReason:   failurefollowup.ExternalRolloutPrerequisitesReason,
		},
		{
			name:         "resolved issue section",
			continuation: "\n\n**Resolved issue:** Deployment tests failed before the fix.\nDeployment tests now pass.",
			wantReason:   failurefollowup.ExternalRolloutPrerequisitesReason,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targetDir := t.TempDir()
			const prompt = "Complete the rollout"
			detailsField := "**Error details:**"
			if tc.detailsHeading {
				detailsField = "## " + detailsField
			}
			output := "Failure: Production rollout remains pending.\n" +
				detailsField + " IAM access and Secrets Manager provisioning are still outstanding.\n" +
				"API flags default to off." + tc.continuation
			h := New(&fakeRunner{t: t, exps: []expectedRun{{
				cmd: codexCommand(targetDir, prompt),
				res: execx.Result{Stdout: output},
			}}})
			err := h.runCodex(context.Background(), agentruntime.Default(), targetDir, prompt, codexRunOptions{}, "", "")
			if err == nil {
				t.Fatal("runCodex() error = nil, want a reported failure")
			}
			if !strings.Contains(err.Error(), " | API flags default to off.") {
				t.Errorf("runCodex() error = %q, want the status continuation", err)
			}
			result := h.fail(ExitCodex, "codex", err, targetDir)
			if got := failurefollowup.NonRemediableFailureReason(result.Err); got != tc.wantReason {
				t.Errorf("NonRemediableFailureReason() = %q, want %q for %q", got, tc.wantReason, err)
			}
		})
	}
}

func TestRunCodexKeepsImplementationFailureFatalWithValidationToolingCaveat(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, summary, details string
	}{
		{
			name:    "missing validation runner in details",
			summary: "Could not complete the implementation.",
			details: "Local test runner unavailable in runtime. vitest: command not found.",
		},
		{
			name:    "validation command in details",
			summary: "Required implementation changes remain unfinished.",
			details: "`npm run build` failed with `sh: 1: tsc: not found`.",
		},
		{
			name:    "validation caveat in summary",
			summary: "Could not update repository files. Local validation tooling unavailable.",
			details: "Writing the required files failed. vitest: command not found.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			targetDir := t.TempDir()
			const prompt = "Update the application"
			h := New(&fakeRunner{t: t, exps: []expectedRun{{
				cmd: codexCommand(targetDir, prompt),
				res: execx.Result{Stdout: "Failure: " + tc.summary + "\nError details: " + tc.details},
			}}})
			err := h.runCodex(context.Background(), agentruntime.Default(), targetDir, prompt, codexRunOptions{}, "", "")
			if err == nil || !strings.Contains(err.Error(), tc.summary) {
				t.Fatalf("runCodex() error = %v, want implementation failure %q", err, tc.summary)
			}
		})
	}
}

func TestRunCodexClassifiesCompleteValidationReport(t *testing.T) {
	t.Parallel()

	const longDetails = "Failure: Local validation tooling unavailable in runtime.\n" +
		"Error details:\n" +
		"Attempted the configured validation command.\n" +
		"The shell started with the runtime PATH.\n" +
		"The command exited before running any tests.\n" +
		"vitest: command not found."
	const smokeReport = "Failure: Local validation command failed.\n" +
		"Error details: The curl smoke command exited with command not found.\n"
	const taskBlocker = "\nCannot safely meet the required coverage target."

	for _, tc := range []struct {
		name, output string
		wantError    bool
		wantBounded  bool
	}{
		{"missing tool after three lines", longDetails, false, false},
		{"task blocker after display limit", longDetails + taskBlocker, true, true},
		{"Go assertion and missing linter", "Failure: go test ./internal/web failed.\nError details: expected status 200, got 500.\nOptional lint: eslint: command not found.", true, false},
		{"Go compiler diagnostic and missing linter", "Failure: go test ./internal/web failed.\nError details: handler.go:12:6: undefined: renderPage\nOptional lint: golangci-lint: command not found.", true, false},
		{"Go build failure and missing linter", "Failure: go test ./internal/web failed.\nError details:\nFAIL\texample.com/project/internal/web [build failed]\nOptional lint: golangci-lint: command not found.", true, false},
		{"fenced Go test failure and missing linter", "Failure: go test ./internal/web failed.\nError details:\n```text\n--- FAIL: TestStatus (0.00s)\n```\nOptional lint: golangci-lint: command not found.", true, false},
		{"JavaScript assertion and missing linter", "Failure: npm test failed.\nError details: AssertionError: expected true to be false.\nOptional lint: eslint: command not found.", true, false},
		{"smoke command in details with failed fallback", smokeReport + "Smoke fallback failed.", true, false},
		{"conflicting smoke fallback results", smokeReport + "Smoke fallback succeeded.\nSmoke fallback failed.", true, false},
		{"smoke only named in fallback", "Failure: Local validation command failed.\nError details: curl: command not found. Smoke fallback failed.", true, false},
		{"smoke command in details with successful fallback", smokeReport + "Smoke fallback succeeded.", false, false},
		{"fallback result on its own line", smokeReport + "Fallback succeeded.", false, false},
		{"unrelated fallback", smokeReport + "Notes: Database fallback succeeded.", true, false},
		{"unrelated smoke success in next section", smokeReport + "Validation: Unrelated smoke fallback succeeded.", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			targetDir := t.TempDir()
			const prompt = "Update the application"
			h := New(&fakeRunner{t: t, exps: []expectedRun{{
				cmd: codexCommand(targetDir, prompt),
				res: execx.Result{Stdout: tc.output},
			}}})
			err := h.runCodex(context.Background(), agentruntime.Default(), targetDir, prompt, codexRunOptions{}, "", "")
			if (err != nil) != tc.wantError {
				t.Fatalf("runCodex() error = %v, want error = %v", err, tc.wantError)
			}
			if tc.wantBounded &&
				(!strings.Contains(err.Error(), truncatedErrorDetails) || strings.Contains(err.Error(), "vitest:")) {
				t.Fatalf("runCodex() error = %q, want bounded error details", err)
			}
		})
	}
}

func TestRunCodexKeepsCodeFailureRemediableAfterValidationSection(t *testing.T) {
	t.Parallel()

	for _, heading := range []string{"Validation:", "**Validation:**", "### Validation", "### Unblock path", "## Next steps", "###\tNotes"} {
		t.Run(heading, func(t *testing.T) {
			t.Parallel()

			targetDir := t.TempDir()
			const prompt = "Update the application"
			output := "Failure: Build failed.\n" +
				"Error details: undefined variable in the application.\n\n" +
				heading + "\nA fixture quotes `quota exceeded`."
			h := New(&fakeRunner{t: t, exps: []expectedRun{{
				cmd: codexCommand(targetDir, prompt),
				res: execx.Result{Stdout: output},
			}}})
			err := h.runCodex(context.Background(), agentruntime.Default(), targetDir, prompt, codexRunOptions{}, "", "")
			if err == nil || !strings.Contains(err.Error(), "undefined variable") {
				t.Fatalf("runCodex() error = %v, want build failure", err)
			}
			result := h.fail(ExitCodex, "codex", err, targetDir)
			if reason := failurefollowup.NonRemediableFailureReason(result.Err); reason != "" {
				t.Fatalf("NonRemediableFailureReason() = %q, want repair eligible for %q", reason, err)
			}
		})
	}
}

func TestCodexExtractErrorDetailRetainsFencedDiagnosticHeadings(t *testing.T) {
	t.Parallel()

	output := "Failure: Build failed.\nError details:\n" +
		"```text\n### Diagnostic output\nValidation: quota exceeded\n```\n" +
		"### Notes\nUnrelated fixture output."
	const want = "Error details:\n### Diagnostic output\nValidation: quota exceeded"
	if detail := codexExtractErrorDetail(output); detail != want {
		t.Fatalf("codexExtractErrorDetail() = %q, want %q", detail, want)
	}
}

func TestCodexExtractErrorDetailRetainsMarkdownSubsections(t *testing.T) {
	t.Parallel()

	output := "Failure: Deployment validation failed.\n" +
		"## Error details:\n### Validation\nDeployment tests failed.\n" +
		"## Next steps\nUnrelated notes."
	detail := codexExtractErrorDetail(output)
	if !strings.Contains(detail, "Deployment tests failed.") || strings.Contains(detail, "Unrelated notes.") {
		t.Fatalf("codexExtractErrorDetail() = %q, want only the nested diagnostic", detail)
	}
}

func TestIsMarkdownCodeFence(t *testing.T) {
	t.Parallel()

	for _, line := range []string{"```", "~~~", "```json", "~~~go", "`"} {
		if !isMarkdownCodeFence(line) {
			t.Fatalf("isMarkdownCodeFence(%q) = false, want true", line)
		}
	}
	for _, line := range []string{"", "```go test ./... failed", "`npm run build` failed", "EROFS: read-only"} {
		if isMarkdownCodeFence(line) {
			t.Fatalf("isMarkdownCodeFence(%q) = true, want false", line)
		}
	}
}

func TestAgentFailureFieldLineStripsMarkdownDecoration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		line string
		want string
	}{
		{"**Failure:** broke", "Failure: broke"},
		{"- **Error details:**", "Error details:"},
		{"> *Failure:* broke", "Failure: broke"},
		{"## Failure", "Failure"},
		{"Failure: broke", "Failure: broke"},
		{`"summary": "Task failed"`, `"summary": "Task failed"`},
	}
	for _, tc := range cases {
		if got := agentFailureFieldLine(tc.line); got != tc.want {
			t.Fatalf("agentFailureFieldLine(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}
