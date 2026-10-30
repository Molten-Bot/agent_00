package app

import (
	"strings"
	"testing"
)

func TestIsNoFailureReportLineClearsSentenceAndClauseSuffixes(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"Failure: None",
		"Failure: None.",
		"Failure: None. All checks passed.",
		"Failure: None, all checks passed.",
		"Failure: None (all checks passed)",
		"Failure: None - all checks passed",
		"Failure: N/A. Nothing to report.",
		"Failure: Nothing. Validation gap: npm unavailable.",
		"Failure: None found",
		"Failure: No issues",
		"Failure: No issues found.",
		"Failure: No known blockers.",
		"Failure: Nothing to report",
		"Failure: Not applicable",
	} {
		if !isNoFailureReportLine(line) {
			t.Fatalf("isNoFailureReportLine(%q) = false, want true", line)
		}
	}

	for _, line := range []string{
		"Failure: none of the tests could run.",
		"Failure: no tests were executed, so the branch is unverified.",
		"Failure: nothing was built because the toolchain is missing.",
		"Failure: build failed.",
		"Failure: no output was produced by the build.",
		"Failure: none of the required checks reported back.",
	} {
		if isNoFailureReportLine(line) {
			t.Fatalf("isNoFailureReportLine(%q) = true, want false", line)
		}
	}
}

// Agents write the report as a markdown heading with the value on the next
// line. The value still decides the run: a bare "**Failure:**" followed by a
// no-failure sentinel must not abort a healthy run, and the same heading
// followed by a real report must still fail it.
func TestCodexReportedFailureInOutputReadsBareFailureHeadingValue(t *testing.T) {
	t.Parallel()

	healthy := strings.Join([]string{
		"Fixed the sandbox config grant and re-ran the focused tests.",
		"Validation: go test ./internal/app passed.",
		"**Failure:**",
		"None - all checks passed.",
	}, "\n")
	if failed, detail := codexReportedFailureInOutput(healthy, true); failed {
		t.Fatalf("healthy run reported as failed: %q", detail)
	}

	reported := strings.Join([]string{
		"Ran the suite.",
		"**Failure:**",
		"go build ./... failed to compile.",
	}, "\n")
	if failed, _ := codexReportedFailureInOutput(reported, true); !failed {
		t.Fatal("bare failure heading with a reported failure was not treated as a failure")
	}
}

func TestCondenseRepeatedCheckRowsKeepsOnlyFinalRenderNotices(t *testing.T) {
	t.Parallel()

	const (
		notice  = "Refreshing checks status every 10 seconds. Press Ctrl+C to quit."
		lintURL = "https://github.com/acme/repo/actions/runs/1/job/10"
		testURL = "https://github.com/acme/repo/actions/runs/1/job/11"
	)

	text := strings.Join([]string{
		notice,
		"",
		"2 failing, 1 successful, and 1 pending checks",
		"",
		"Lint & Format\tfail\t30s\t" + lintURL + "\t",
		"Test\tpending\t0\t" + testURL + "\t",
		notice,
		"",
		"1 failing, 2 successful checks",
		"",
		"Lint & Format\tfail\t31s\t" + lintURL + "\t",
		"Test\tpass\t37s\t" + testURL + "\t",
	}, "\n")

	want := strings.Join([]string{
		notice,
		"1 failing, 2 successful checks",
		"Lint & Format\tfail\t31s\t" + lintURL + "\t",
		"Test\tpass\t37s\t" + testURL + "\t",
	}, "\n")
	if got := condenseRepeatedCheckRows(text); got != want {
		t.Fatalf("condenseRepeatedCheckRows() = %q, want %q", got, want)
	}

	single := strings.Join([]string{
		notice,
		"1 failing, 2 successful checks",
		"Lint & Format\tfail\t31s\t" + lintURL + "\t",
	}, "\n")
	if got := condenseRepeatedCheckRows(single); got != "" {
		t.Fatalf("condenseRepeatedCheckRows(single render) = %q, want empty", got)
	}
}
