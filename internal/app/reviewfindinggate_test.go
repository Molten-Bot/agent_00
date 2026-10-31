package app

import "testing"

func TestReviewFindingIsBlockingBySeverity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		severity string
		blocking bool
	}{
		{severity: "Critical", blocking: true},
		{severity: "high", blocking: true},
		{severity: " Medium ", blocking: true},
		{severity: "Low", blocking: false},
		{severity: "nit", blocking: false},
		{severity: "Informational", blocking: false},
		{severity: "suggestion", blocking: false},
		{severity: "", blocking: true},
		{severity: "unlabelled", blocking: true},
	}
	for _, tc := range cases {
		if got := reviewFindingIsBlocking(reviewFinding{Severity: tc.severity}); got != tc.blocking {
			t.Fatalf("reviewFindingIsBlocking(%q) = %t, want %t", tc.severity, got, tc.blocking)
		}
	}
}

func TestBlockingReviewFindingsDropsPolishAndKeepsOrder(t *testing.T) {
	t.Parallel()
	findings := []reviewFinding{
		{Severity: "Low", Title: "no retry on one fetch"},
		{Severity: "High", Title: "wrong exit code"},
		{Severity: "nit", Title: "comment is long"},
		{Severity: "", Title: "unlabelled"},
	}

	blocking := blockingReviewFindings(findings)
	if len(blocking) != 2 {
		t.Fatalf("blockingReviewFindings returned %d findings, want 2: %+v", len(blocking), blocking)
	}
	if blocking[0].Title != "wrong exit code" || blocking[1].Title != "unlabelled" {
		t.Fatalf("unexpected blocking findings: %+v", blocking)
	}
}

// The failing run that motivated this gate ended with three Low findings and a
// review summary of "no blocking correctness issues", yet exhausted every fix
// pass and failed the task. Low-only outcomes must now read as releasable.
func TestBlockingReviewFindingsEmptyForLowOnlyOutcome(t *testing.T) {
	t.Parallel()
	outcome := reviewOutcome{
		Status:  "findings",
		Summary: "only three Low polish findings, no blocking correctness issues",
		Findings: []reviewFinding{
			{Severity: "Low", Path: "a.sh", Line: 77, Title: "bundle fetch has no retry"},
			{Severity: "Low", Path: "a.sh", Line: 1, Title: "script committed non-executable"},
			{Severity: "Low", Path: "a.sh", Line: 38, Title: "narrative comment block is long"},
		},
	}

	if blocking := blockingReviewFindings(outcome.Findings); len(blocking) != 0 {
		t.Fatalf("low-only outcome reported %d blocking findings: %+v", len(blocking), blocking)
	}
}
