package app

import (
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestSummarizeCheckOutputCollapsesWatchRepeats(t *testing.T) {
	t.Parallel()

	row := func(name, bucket string) string {
		return name + "\t" + bucket + "\t3s\thttps://example.com/job\t"
	}
	watch := strings.Join([]string{
		row("Lint", "pending"),
		row("Test", "pending"),
		row("Lint", "fail"),
		row("Test", "pass"),
	}, "\n")

	summary := summarizeCheckOutput(execx.Result{Stdout: watch})
	if got := strings.Count(summary, "Lint\t"); got != 1 {
		t.Fatalf("Lint appears %d times in summary:\n%s", got, summary)
	}
	if !strings.Contains(summary, row("Lint", "fail")) {
		t.Fatalf("latest Lint row missing from summary:\n%s", summary)
	}
	if !strings.Contains(summary, row("Test", "pass")) {
		t.Fatalf("latest Test row missing from summary:\n%s", summary)
	}
	if strings.Contains(summary, "pending") {
		t.Fatalf("stale pending rows kept in summary:\n%s", summary)
	}
}

func TestSummarizeCheckOutputKeepsSingleRenderUnchanged(t *testing.T) {
	t.Parallel()

	single := "Lint\tfail\t3s\thttps://example.com/lint\t\nTest\tpass\t9s\thttps://example.com/test\t"
	if got := summarizeCheckOutput(execx.Result{Stdout: single}); got != single {
		t.Fatalf("summary = %q, want %q", got, single)
	}
	if got := summarizeCheckOutput(execx.Result{}); got != "No check output was provided by gh." {
		t.Fatalf("empty summary = %q", got)
	}
}

func TestSummarizeCheckOutputKeepsNonTabularNotices(t *testing.T) {
	t.Parallel()

	summary := summarizeCheckOutput(execx.Result{
		Stdout: "Lint\tpending\t0\thttps://example.com/job\t\nLint\tfail\t3s\thttps://example.com/job\t",
		Stderr: "Some checks were not successful",
	})
	if !strings.Contains(summary, "Some checks were not successful") {
		t.Fatalf("gh notice dropped from summary:\n%s", summary)
	}
}

// Agents format their final message as Markdown, so the contract-mandated
// "Failure:" heading almost always arrives bolded.
func TestCodexReportedFailureAcceptsMarkdownHeadings(t *testing.T) {
	t.Parallel()

	res := execx.Result{Stdout: strings.Join([]string{
		"Done. Here's where things stand.",
		"## Failure report",
		"**Failure:** Could not verify the Lint & Format fix locally.",
		"**Error details:** Every shell invocation in this runtime dies before running.",
	}, "\n")}

	failed, detail := codexReportedFailure(res)
	if !failed {
		t.Fatal("markdown-bolded Failure heading was not reported as a failure")
	}
	if !strings.HasPrefix(detail, "Failure: Could not verify") {
		t.Fatalf("detail = %q", detail)
	}
	if full := codexFailureDetailWithErrorDetails(res, detail); !strings.Contains(full, "Error details: Every shell invocation") {
		t.Fatalf("error details missing from %q", full)
	}
}

func TestCodexReportedFailureIgnoresBareMarkdownHeadingAfterCompletion(t *testing.T) {
	t.Parallel()

	res := execx.Result{Stdout: strings.Join([]string{
		"Done.",
		"Changed:",
		"- internal/app/harness.go",
		"**Failure:**",
		"None.",
	}, "\n")}

	if failed, detail := codexReportedFailure(res); failed {
		t.Fatalf("bare failure heading after completion reported as failure: %q", detail)
	}
}
