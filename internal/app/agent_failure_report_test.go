package app

import (
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

// Agents close a healthy run with an explicit "**Failure:** None" line. Once
// stripMarkdownEmphasis folds that spelling onto the bare "Failure:" prefix,
// the value after the heading is the only thing separating a successful run
// from an aborted one.
func TestCodexReportedFailureReadsTheValueAfterTheFailureHeading(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		res        execx.Result
		wantFailed bool
	}{
		{
			name:       "bold no-failure value on stdout",
			res:        execx.Result{Stdout: "Done.\n**Failure:** None\n**Error details:** None\n"},
			wantFailed: false,
		},
		{
			name:       "bold no-failure value with trailing clause",
			res:        execx.Result{Stdout: "Done.\n**Failure:** None - all checks passed\n"},
			wantFailed: false,
		},
		{
			name:       "plain no-failure value on stderr",
			res:        execx.Result{Stderr: "Failure: n/a\n"},
			wantFailed: false,
		},
		{
			name:       "bold real failure on stdout",
			res:        execx.Result{Stdout: "**Failure:** could not clone the repository\n"},
			wantFailed: true,
		},
		{
			name:       "no-failure prefix in a real report stays fatal",
			res:        execx.Result{Stdout: "**Failure:** none of the tests could run\n"},
			wantFailed: true,
		},
		{
			name:       "bold real failure on stderr",
			res:        execx.Result{Stderr: "**Failure:** build command missing\n"},
			wantFailed: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			failed, detail := codexReportedFailure(tc.res)
			if failed != tc.wantFailed {
				t.Fatalf("codexReportedFailure() = (%t, %q), want failed=%t", failed, detail, tc.wantFailed)
			}
		})
	}
}

// `gh pr checks --watch` re-renders the whole table every refresh, so identical
// rows must collapse. Two checks that merely share a display name are distinct
// checks with their own job URLs, and collapsing those would drop a failing row
// from the remediation summary.
func TestCondenseRepeatedCheckRowsKeepsDistinctSameNamedChecks(t *testing.T) {
	t.Parallel()

	repeated := strings.Join([]string{
		"All Checks Passed\tfail\t3s\thttps://github.com/acme/repo/actions/runs/1/job/10",
		"Lint & Format\tfail\t30s\thttps://github.com/acme/repo/actions/runs/1/job/11",
		"All Checks Passed\tfail\t3s\thttps://github.com/acme/repo/actions/runs/1/job/10",
		"Lint & Format\tfail\t30s\thttps://github.com/acme/repo/actions/runs/1/job/11",
	}, "\n")
	want := strings.Join([]string{
		"All Checks Passed\tfail\t3s\thttps://github.com/acme/repo/actions/runs/1/job/10",
		"Lint & Format\tfail\t30s\thttps://github.com/acme/repo/actions/runs/1/job/11",
	}, "\n")
	if got := condenseRepeatedCheckRows(repeated); got != want {
		t.Fatalf("condenseRepeatedCheckRows() = %q, want %q", got, want)
	}

	distinct := strings.Join([]string{
		"Build and test\tfail\t23s\thttps://github.com/acme/repo/actions/runs/1/job/11",
		"Build and test\tpass\t22s\thttps://github.com/acme/repo/actions/runs/2/job/22",
	}, "\n")
	if got := condenseRepeatedCheckRows(distinct); got != "" {
		t.Fatalf("condenseRepeatedCheckRows() = %q, want no condensing for distinct checks", got)
	}
	summary := summarizeCheckOutput(execx.Result{Stdout: distinct + "\n"})
	if !strings.Contains(summary, "Build and test\tfail\t23s") {
		t.Fatalf("summarizeCheckOutput() dropped the failing check: %q", summary)
	}
}

// gh pads every check row to the same column count, so a row whose last column
// is empty ends in a tab. Trimming that padding off the final row rewrote the
// table that the remediation prompt and the "required PR checks failed" error
// carry, so the summary stopped matching gh's own output.
func TestSummarizeCheckOutputPreservesTrailingColumnPadding(t *testing.T) {
	t.Parallel()

	row := func(name, bucket, url string) string {
		return name + "\t" + bucket + "\t3s\t" + url + "\t"
	}

	// Condensed path: the surviving row keeps its padding, including when gh
	// terminates the watch output with a newline.
	watched := strings.Join([]string{
		row("Build and test", "pending", "https://example.com/job/1"),
		row("Build and test", "fail", "https://example.com/job/1"),
	}, "\n")
	want := row("Build and test", "fail", "https://example.com/job/1")
	if got := summarizeCheckOutput(execx.Result{Stdout: watched + "\n"}); got != want {
		t.Fatalf("summarizeCheckOutput(watched) = %q, want %q", got, want)
	}

	// Single-render path: the row comes back byte for byte.
	single := row("Lint", "fail", "https://example.com/job/2")
	if got := summarizeCheckOutput(execx.Result{Stdout: single + "\n"}); got != single {
		t.Fatalf("summarizeCheckOutput(single) = %q, want %q", got, single)
	}

	// Whitespace-only output still counts as no output at all.
	if got := summarizeCheckOutput(execx.Result{Stdout: " \n\t\n"}); got != "No check output was provided by gh." {
		t.Fatalf("summarizeCheckOutput(blank) = %q", got)
	}
}
