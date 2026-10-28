package app

import (
	"strings"
	"testing"
)

// The auto-merge gate and the post-task review cycle must agree about which
// severities block. A severity the harness cannot classify stays blocking on
// both paths; anything the advisory list names is waved through on both.
func TestReviewOutcomeAutoMergeSharesBlockingClassifier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		severity string
		want     bool
	}{
		{name: "low", severity: "Low", want: true},
		{name: "nit", severity: "nit", want: true},
		{name: "medium", severity: "Medium", want: false},
		{name: "critical", severity: "Critical", want: false},
		{name: "unlabeled", severity: "", want: false},
		{name: "unrecognized", severity: "major", want: false},
		{name: "blocker", severity: "blocker", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			outcome := reviewOutcome{
				Status:     "merge-ready",
				MergeReady: true,
				Findings:   []reviewFinding{{Severity: tc.severity, Title: "something"}},
			}
			if got := reviewOutcomeAllowsAutoMerge(outcome); got != tc.want {
				t.Fatalf("reviewOutcomeAllowsAutoMerge(%q) = %t, want %t", tc.severity, got, tc.want)
			}
			blocking := blockingReviewFindings(outcome.Findings)
			if gotBlocking := len(blocking) == 0; gotBlocking != tc.want {
				t.Fatalf("blockingReviewFindings(%q) empty = %t, want %t", tc.severity, gotBlocking, tc.want)
			}
		})
	}
}

func TestReviewOutcomeAutoMergeStillRequiresCleanStatus(t *testing.T) {
	t.Parallel()

	if reviewOutcomeAllowsAutoMerge(reviewOutcome{Status: "clean", MergeReady: false}) {
		t.Fatal("auto-merge must require mergeReady")
	}
	if reviewOutcomeAllowsAutoMerge(reviewOutcome{Status: "blocked", MergeReady: true}) {
		t.Fatal("auto-merge must require a clean status")
	}
	if !reviewOutcomeAllowsAutoMerge(reviewOutcome{Status: "clean", MergeReady: true}) {
		t.Fatal("clean merge-ready outcome must allow auto-merge")
	}
}

func TestSplitReviewFindingsPartitionsInOrder(t *testing.T) {
	t.Parallel()

	findings := []reviewFinding{
		{Severity: "Low", Title: "first advisory"},
		{Severity: "High", Title: "first blocking"},
		{Severity: "nitpick", Title: "second advisory"},
		{Severity: "", Title: "unlabeled blocking"},
	}
	blocking, advisory := splitReviewFindings(findings)
	if len(blocking) != 2 || blocking[0].Title != "first blocking" || blocking[1].Title != "unlabeled blocking" {
		t.Fatalf("blocking = %+v", blocking)
	}
	if len(advisory) != 2 || advisory[0].Title != "first advisory" || advisory[1].Title != "second advisory" {
		t.Fatalf("advisory = %+v", advisory)
	}
	if len(blocking)+len(advisory) != len(findings) {
		t.Fatalf("split lost findings: %d + %d != %d", len(blocking), len(advisory), len(findings))
	}
}

// Findings without a title render to nothing, so the overflow count has to be
// derived from the points actually rendered. Counting against the limit hid
// them: the summary claimed fewer unnamed findings than it dropped.
func TestSummarizeReviewFindingsCountsDroppedTitlelessFindings(t *testing.T) {
	t.Parallel()

	findings := []reviewFinding{
		{Severity: "High", Path: "a.go", Line: 1, Title: "first"},
		{Severity: "High", Title: ""},
		{Severity: "High", Title: "   "},
		{Severity: "High", Path: "b.go", Title: "second"},
	}
	summary := summarizeReviewFindings(findings, 6)
	if !strings.Contains(summary, "[High] a.go:1 - first") || !strings.Contains(summary, "[High] b.go - second") {
		t.Fatalf("summary dropped rendered findings: %q", summary)
	}
	if !strings.Contains(summary, "+2 more") {
		t.Fatalf("summary = %q, want +2 more for the title-less findings", summary)
	}
}

func TestSummarizeReviewFindingsCountsLimitOverflow(t *testing.T) {
	t.Parallel()

	findings := make([]reviewFinding, 0, 8)
	for i := 0; i < 8; i++ {
		findings = append(findings, reviewFinding{Severity: "High", Title: "finding"})
	}
	if got := summarizeReviewFindings(findings, 6); !strings.Contains(got, "+2 more") {
		t.Fatalf("summarizeReviewFindings() = %q, want +2 more", got)
	}
	if got := summarizeReviewFindings(findings[:3], 6); strings.Contains(got, "more") {
		t.Fatalf("summarizeReviewFindings() = %q, want no overflow suffix", got)
	}
	if got := summarizeReviewFindings(nil, 6); got != "no finding details were returned" {
		t.Fatalf("summarizeReviewFindings(nil) = %q", got)
	}
}

// An unused fix budget should be spent once on advisory findings instead of
// discarding them, but never more than once and never on the confirmation
// pass, which has no budget of its own.
func TestAdvisoryFixPassAllowedSpendsUnusedBudgetOnce(t *testing.T) {
	t.Parallel()

	advisory := []reviewFinding{{Severity: "Low", Title: "tidy this"}}
	if !advisoryFixPassAllowed(advisory, false, 1, 6) {
		t.Fatal("first advisory-only pass must be allowed when the budget is unused")
	}
	if advisoryFixPassAllowed(advisory, true, 2, 6) {
		t.Fatal("advisory findings must get at most one fix pass")
	}
	if advisoryFixPassAllowed(nil, false, 1, 6) {
		t.Fatal("no advisory findings means no advisory pass")
	}
	if advisoryFixPassAllowed(advisory, false, 7, 6) {
		t.Fatal("the confirmation pass has no fix budget to spend")
	}
	if !advisoryFixPassAllowed(advisory, false, 6, 6) {
		t.Fatal("the final budgeted pass may still repair advisory findings")
	}
}

// A blocking fix pass is already paying for an agent invocation, so the
// advisory findings it used to drop ride along in the same prompt.
func TestFinalReviewFixPromptCarriesAdvisoryFindings(t *testing.T) {
	t.Parallel()

	repo := repoWorkspace{PRURL: "https://github.com/example/repo/pull/7", Branch: "agent00-fix"}
	findings := []reviewFinding{
		{Severity: "High", Path: "a.go", Line: 12, Title: "blocking defect"},
		{Severity: "Low", Path: "b.go", Line: 3, Title: "advisory nit"},
	}
	prompt := finalReviewFixPrompt("original task", repo, findings, 1, 6)
	for _, want := range []string{
		"1. [High] a.go:12 - blocking defect",
		"2. [Low] b.go:3 - advisory nit",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("fix prompt missing %q:\n%s", want, prompt)
		}
	}
}
