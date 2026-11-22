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

func TestCodexFailureDetailIgnoresStderrPromptEcho(t *testing.T) {
	t.Parallel()

	const summary = "Failure: Could not complete the implementation."
	const privateText = "PRIVATE TASK TEXT"
	for _, stderr := range []string{
		"OpenAI Codex\nuser\nError details:\n" + privateText + "\ncodex\n" + summary,
		"user\n" + summary + "\nError details:\n- " + privateText,
		"Error details: <failing command or blocker>\n" + privateText,
		summary + "\nError details: <failing command or blocker>\nuser\n" + privateText,
	} {
		display, full := codexFailureDetailWithErrorDetails(execx.Result{Stdout: summary, Stderr: stderr}, summary)
		if display != summary || full != summary {
			t.Errorf("failure detail = (%q, %q), want only the failure summary", display, full)
		}
	}
}

func TestCodexFailureDetailRetainsCompactStderrReport(t *testing.T) {
	t.Parallel()

	const summary = "Failure: Could not complete the implementation."
	const stderr = summary + "\nError details:\nAssertionError: expected status 200, got 500."
	_, full := codexFailureDetailWithErrorDetails(execx.Result{Stderr: stderr}, summary)
	if full != stderr {
		t.Errorf("full failure detail = %q, want %q", full, stderr)
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
			name:         "deployment test failure under a heading in a plain field",
			continuation: "\n\n### Deployment tests\nDeployment tests fail.",
		},
		{
			name:         "deployment test failure in an inline validation field",
			continuation: "\nValidation: Deployment tests failed.",
		},
		{
			name:         "deployment test failure under validation in a plain field",
			continuation: "\n\n### Validation\nDeployment tests failed.",
		},
		{
			name:         "deployment test failure under a heading quotes quota",
			continuation: "\n\n### Deployment tests\nAssertionError: expected `quota exceeded`, got HTTP 500.",
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
			detailsField := "Error details:"
			if tc.detailsHeading {
				detailsField = "## **Error details:**"
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
		{"Jest exit and missing optional linter", "Failure: npm test failed.\nError details: Jest exited with status 1.\nOptional lint: eslint: command not found.", true, false},
		{"Jest exit and missing linter on the same line", "Failure: npm test failed.\nError details: Jest exited with status 1. Optional lint: eslint: command not found.", true, false},
		{"test command exit explained by missing runner", "Failure: npm test failed.\nError details: npm test exited with status 1 because vitest: command not found.", false, false},
		{"JavaScript assertion and missing linter", "Failure: npm test failed.\nError details: AssertionError: expected true to be false.\nOptional lint: eslint: command not found.", true, false},
		{"smoke command in details with failed fallback", smokeReport + "Smoke fallback failed.", true, false},
		{"conflicting smoke fallback results", smokeReport + "Smoke fallback succeeded.\nSmoke fallback failed.", true, false},
		{"fallback starts server but endpoint fails", smokeReport + "Fallback succeeded in starting the server, but the endpoint returned HTTP 503.", true, false},
		{"fallback success contradicted by endpoint", smokeReport + "Fallback succeeded.\nThe endpoint returned HTTP 503.", true, false},
		{"fallback success contradicted by failed check", smokeReport + "Fallback succeeded.\nFallback smoke check failed.", true, false},
		{"only server startup succeeded", smokeReport + "Fallback succeeded in starting the server.", true, false},
		{"smoke only named in fallback", "Failure: Local validation command failed.\nError details: curl: command not found. Smoke fallback failed.", true, false},
		{"smoke command in details with successful fallback", smokeReport + "Smoke fallback succeeded.", false, false},
		{"fallback result on its own line", smokeReport + "Fallback succeeded.", false, false},
		{"expected unauthorized response", smokeReport + "Fallback smoke check passed. Endpoint returned expected HTTP 401.", false, false},
		{"unauthorized response as expected", smokeReport + "Fallback smoke check passed. Endpoint returned HTTP 401 as expected.", false, false},
		{"expected status does not mask unexpected response", smokeReport + "Fallback smoke check passed. Endpoint expected HTTP 401 but returned HTTP 503.", true, false},
		{"fallback smoke check passed with Python", smokeReport + "Fallback smoke check passed using Python. Endpoint returned HTTP 200.", false, false},
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
			if err == nil || !strings.Contains(err.Error(), "undefined variable") || strings.Contains(err.Error(), "A fixture quotes") {
				t.Fatalf("runCodex() error = %v, want build failure without the separate section", err)
			}
			result := h.fail(ExitCodex, "codex", err, targetDir)
			if reason := failurefollowup.NonRemediableFailureReason(result.Err); reason != "" {
				t.Fatalf("NonRemediableFailureReason() = %q, want repair eligible for %q", reason, err)
			}
		})
	}
}

func TestRunCodexClassifiesQuotaEvidenceInErrorDetails(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, summary, details, evidence, wantReason string
	}{
		{
			name:     "quoted quota in a continued assertion",
			summary:  "Deployment tests failed.",
			details:  "The readiness check returned HTTP 500.\nAssertionError: expected `quota exceeded` to be mapped to HTTP 429.",
			evidence: "AssertionError:",
		},
		{
			name:     "quota and billing text in Go test output",
			summary:  "Go tests failed.",
			details:  "\n```text\n--- FAIL: TestQuotaMessage (0.00s)\n    handler_test.go:42: response omitted quota exceeded. Check your plan and billing details.\n```",
			evidence: "quota exceeded",
		},
		{
			name:       "actual quota blocker in details",
			summary:    "Could not finish the run.",
			details:    "The provider request failed.\nERROR: Quota exceeded. Check your plan and billing details.",
			evidence:   "ERROR: Quota exceeded",
			wantReason: "quota exceeded",
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
			if err == nil || !strings.Contains(err.Error(), tc.evidence) {
				t.Fatalf("runCodex() error = %v, want diagnostic %q", err, tc.evidence)
			}
			result := h.fail(ExitCodex, "codex", err, targetDir)
			if got := failurefollowup.NonRemediableFailureReason(result.Err); got != tc.wantReason {
				t.Errorf("NonRemediableFailureReason() = %q, want %q for %q", got, tc.wantReason, err)
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

	for _, field := range []string{"Error details:", "## Error details:"} {
		output := "Failure: Deployment validation failed.\n" + field +
			"\n### Validation\nDeployment tests failed.\n" +
			"## Next steps\nUnrelated notes."
		const want = "Error details:\n### Validation\nDeployment tests failed."
		if detail := codexExtractErrorDetail(output); detail != want {
			t.Errorf("codexExtractErrorDetail() with %q = %q, want %q", field, detail, want)
		}
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
