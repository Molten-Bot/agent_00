package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

// ---- Run: guard clauses and default wiring ---------------------------------

func TestRunNilGuardsAndDefaultPollInterval(t *testing.T) {
	t.Parallel()

	var nilMonitor *PRMergeMonitor
	if err := nilMonitor.Run(context.Background()); err != nil {
		t.Fatalf("nil monitor Run() error = %v, want nil", err)
	}
	if err := (&PRMergeMonitor{}).Run(context.Background()); err != nil {
		t.Fatalf("Run(no broker, no runner) error = %v, want nil", err)
	}
	if err := (&PRMergeMonitor{Broker: NewBroker()}).Run(context.Background()); err != nil {
		t.Fatalf("Run(no runner) error = %v, want nil", err)
	}

	monitor := &PRMergeMonitor{
		Broker: NewBroker(),
		Runner: &stubPRMonitorRunner{result: execx.Result{Stdout: `{}`}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Run so the loop exits right after the first pollOnce
	if err := monitor.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if monitor.PollInterval != defaultPRMergePollInterval {
		t.Fatalf("PollInterval after Run() = %s, want default %s", monitor.PollInterval, defaultPRMergePollInterval)
	}
}

// ---- shouldMonitorTaskPR / beginCheck / markMerged / forgetMissingTasks ----

func TestShouldMonitorTaskPR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		task Task
		want bool
	}{
		{"no pr url", Task{Status: "completed", PRURL: ""}, false},
		{"completed with pr url", Task{Status: "completed", PRURL: "url"}, true},
		{"no_changes with pr url", Task{Status: "no_changes", PRURL: "url"}, true},
		{"running with pr url", Task{Status: "running", PRURL: "url"}, false},
		{"errored with pr url", Task{Status: "error", PRURL: "url"}, false},
	}
	for _, tt := range tests {
		if got := shouldMonitorTaskPR(tt.task); got != tt.want {
			t.Fatalf("%s: shouldMonitorTaskPR() = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestBeginCheckRejectsBlankInFlightAndMergedRequests(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{}
	if m.beginCheck("") {
		t.Fatal("beginCheck(blank) = true, want false")
	}
	if !m.beginCheck("req-1") {
		t.Fatal("beginCheck(first) = false, want true")
	}
	if m.beginCheck("req-1") {
		t.Fatal("beginCheck(already in flight) = true, want false")
	}
	m.endCheck("req-1")
	if !m.beginCheck("req-1") {
		t.Fatal("beginCheck(after endCheck) = false, want true")
	}
	m.endCheck("req-1")
	m.markMerged("req-1")
	if m.beginCheck("req-1") {
		t.Fatal("beginCheck(merged) = true, want false")
	}
}

func TestMarkMergedIgnoresBlankRequestID(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{}
	m.markMerged("")
	if len(m.merged) != 0 {
		t.Fatalf("len(merged) after blank markMerged = %d, want 0", len(m.merged))
	}
	m.markMerged("req-1")
	if _, ok := m.merged["req-1"]; !ok {
		t.Fatal("merged[req-1] missing after markMerged")
	}
}

func TestForgetMissingTasksPrunesStaleEntries(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{
		merged:         map[string]struct{}{"req-old": {}, "req-keep": {}},
		feedbackQueued: map[string]string{"pr-old": "d1", "pr-keep": "d2"},
	}
	m.forgetMissingTasks(map[string]struct{}{"req-keep": {}}, map[string]struct{}{"pr-keep": {}})

	if _, ok := m.merged["req-old"]; ok {
		t.Fatal("merged[req-old] still present, want pruned")
	}
	if _, ok := m.merged["req-keep"]; !ok {
		t.Fatal("merged[req-keep] missing, want retained")
	}
	if _, ok := m.feedbackQueued["pr-old"]; ok {
		t.Fatal("feedbackQueued[pr-old] still present, want pruned")
	}
	if _, ok := m.feedbackQueued["pr-keep"]; !ok {
		t.Fatal("feedbackQueued[pr-keep] missing, want retained")
	}
}

// ---- checkTaskPR: MarkTaskPRMerged error paths -----------------------------

func TestCheckTaskPRMarksMergedWhenTaskNotFoundInBroker(t *testing.T) {
	t.Parallel()

	broker := NewBroker() // no task recorded for req-missing
	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z"}`}}
	monitor := &PRMergeMonitor{Runner: runner, Broker: broker, Logf: func(string, ...any) {}}

	monitor.checkTaskPR(context.Background(), Task{RequestID: "req-missing", PRURL: "https://github.com/acme/repo/pull/5"})

	monitor.mu.Lock()
	_, ok := monitor.merged["req-missing"]
	monitor.mu.Unlock()
	if !ok {
		t.Fatal("merged[req-missing] missing, want set after ErrTaskNotFound from MarkTaskPRMerged")
	}
}

func TestCheckTaskPRLogsWarnAndSkipsMarkMergedWhenTaskNotCompleted(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.IngestLog("dispatch status=start request_id=req-running repo=acme/repo")

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z"}`}}
	logs := make(chan string, 4)
	monitor := &PRMergeMonitor{
		Runner: runner,
		Broker: broker,
		Logf: func(format string, args ...any) {
			select {
			case logs <- fmt.Sprintf(format, args...):
			default:
			}
		},
	}

	task, ok := broker.Task("req-running")
	if !ok {
		t.Fatal("broker.Task(req-running) found = false")
	}
	task.PRURL = "https://github.com/acme/repo/pull/6"

	monitor.checkTaskPR(context.Background(), task)

	monitor.mu.Lock()
	_, merged := monitor.merged["req-running"]
	monitor.mu.Unlock()
	if merged {
		t.Fatal("merged[req-running] present, want absent (ErrTaskNotCompleted must not call markMerged)")
	}

	select {
	case line := <-logs:
		if !strings.Contains(line, "pr_monitor_mark_merged") {
			t.Fatalf("log line = %q, want pr_monitor_mark_merged warning", line)
		}
	default:
		t.Fatal("expected a warning log line for the ErrTaskNotCompleted path")
	}
}

func TestCheckTaskPRLogsWarnWhenCleanupTaskFails(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.RecordTaskRunConfig("req-cleanup-fails", []byte(`{"repo":"acme/repo","prompt":"ship it"}`))
	broker.IngestLog("dispatch status=start request_id=req-cleanup-fails repo=acme/repo")
	broker.IngestLog("dispatch status=completed request_id=req-cleanup-fails workspace=/tmp/run branch=agent00-ship pr_url=https://github.com/acme/repo/pull/44")

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z"}`}}
	logs := make(chan string, 4)
	monitor := &PRMergeMonitor{
		Runner: runner,
		Broker: broker,
		Logf: func(format string, args ...any) {
			select {
			case logs <- fmt.Sprintf(format, args...):
			default:
			}
		},
		CleanupTask: func(context.Context, string) error {
			return errors.New("cleanup failed")
		},
	}

	task, ok := broker.Task("req-cleanup-fails")
	if !ok {
		t.Fatal("broker.Task() found = false")
	}
	monitor.checkTaskPR(context.Background(), task)

	monitor.mu.Lock()
	_, merged := monitor.merged["req-cleanup-fails"]
	monitor.mu.Unlock()
	if !merged {
		t.Fatal("merged[req-cleanup-fails] missing, want set even when cleanup fails")
	}

	select {
	case line := <-logs:
		if !strings.Contains(line, "pr_monitor_cleanup") {
			t.Fatalf("log line = %q, want pr_monitor_cleanup warning", line)
		}
	default:
		t.Fatal("expected a warning log line for the failed cleanup")
	}
}

func TestDeleteMergedBranchStripsForkOwnerPrefixFromBranchName(t *testing.T) {
	t.Parallel()

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{}`}}
	m := &PRMergeMonitor{Runner: runner}

	err := m.deleteMergedBranch(context.Background(), Task{PRURL: "https://github.com/acme/repo/pull/1"}, prViewState{
		HeadRefName: "contributor:feature",
		BaseRefName: "main",
		HeadRepository: prRepository{
			Name:  "repo",
			Owner: prActor{Login: "acme"},
		},
	})
	if err != nil {
		t.Fatalf("deleteMergedBranch() error = %v", err)
	}

	commands := runner.Commands()
	if len(commands) != 1 {
		t.Fatalf("len(commands) = %d, want 1", len(commands))
	}
	if got, want := commands[0].Args, []string{"api", "-X", "DELETE", "repos/acme/repo/git/refs/heads/feature"}; !equalStringSlices(got, want) {
		t.Fatalf("delete command args = %v, want %v (fork owner prefix must be stripped)", got, want)
	}
}

func TestDeleteMergedBranchSuppressesMissingRefError(t *testing.T) {
	t.Parallel()

	runner := &stubPRMonitorRunner{err: errors.New("GraphQL: Reference does not exist")}
	m := &PRMergeMonitor{Runner: runner}

	err := m.deleteMergedBranch(context.Background(), Task{PRURL: "https://github.com/acme/repo/pull/1", Branch: "agent00-ship"}, prViewState{})
	if err != nil {
		t.Fatalf("deleteMergedBranch() error = %v, want nil (missing ref error must be suppressed)", err)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- deleteMergedBranchesEnabled -------------------------------------------

func TestDeleteMergedBranchesEnabledPrefersFuncOverBoolField(t *testing.T) {
	t.Parallel()

	if (&PRMergeMonitor{DeleteMergedBranches: true}).deleteMergedBranchesEnabled() != true {
		t.Fatal("deleteMergedBranchesEnabled(bool field true) = false, want true")
	}
	if (&PRMergeMonitor{DeleteMergedBranches: false}).deleteMergedBranchesEnabled() != false {
		t.Fatal("deleteMergedBranchesEnabled(bool field false) = true, want false")
	}
	monitor := &PRMergeMonitor{
		DeleteMergedBranches:        true,
		DeleteMergedBranchesEnabled: func() bool { return false },
	}
	if monitor.deleteMergedBranchesEnabled() != false {
		t.Fatal("deleteMergedBranchesEnabled() = true, want false (func must override bool field)")
	}
}

// ---- deleteMergedBranch error paths -----------------------------------------

func TestDeleteMergedBranchValidatesRepoBranchAndBaseBranch(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{Runner: &stubPRMonitorRunner{}}

	if err := m.deleteMergedBranch(context.Background(), Task{PRURL: "not-a-pr-url"}, prViewState{}); err == nil || !strings.Contains(err.Error(), "head repository is required") {
		t.Fatalf("deleteMergedBranch(no repo) error = %v, want head repository is required", err)
	}

	if err := m.deleteMergedBranch(context.Background(), Task{PRURL: "https://github.com/acme/repo/pull/1"}, prViewState{}); err == nil || !strings.Contains(err.Error(), "head branch is required") {
		t.Fatalf("deleteMergedBranch(no branch) error = %v, want head branch is required", err)
	}

	err := m.deleteMergedBranch(context.Background(), Task{PRURL: "https://github.com/acme/repo/pull/1", Branch: "main"}, prViewState{HeadRefName: "main", BaseRefName: "main"})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete base branch") {
		t.Fatalf("deleteMergedBranch(base branch) error = %v, want refusing to delete base branch", err)
	}
}

// ---- isMissingGitHubRefError ------------------------------------------------

func TestIsMissingGitHubRefError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("GraphQL: Reference does not exist"), true},
		{errors.New("HTTP 404: Not Found (HTTP 404)"), true},
		{errors.New("some other failure"), false},
	}
	for _, tt := range tests {
		if got := isMissingGitHubRefError(tt.err); got != tt.want {
			t.Fatalf("isMissingGitHubRefError(%v) = %t, want %t", tt.err, got, tt.want)
		}
	}
}

// ---- HeadRepositoryNameWithOwner --------------------------------------------

func TestHeadRepositoryNameWithOwnerFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state prViewState
		want  string
	}{
		{
			name:  "uses NameWithOwner when present",
			state: prViewState{HeadRepository: prRepository{NameWithOwner: "acme/repo"}},
			want:  "acme/repo",
		},
		{
			name:  "falls back to repository owner login and name",
			state: prViewState{HeadRepository: prRepository{Name: "repo", Owner: prActor{Login: "acme"}}},
			want:  "acme/repo",
		},
		{
			name:  "falls back to headRepositoryOwner when repository owner is blank",
			state: prViewState{HeadRepository: prRepository{Name: "repo"}, HeadRepoOwner: prActor{Login: "contributor"}},
			want:  "contributor/repo",
		},
		{
			name:  "missing owner yields empty",
			state: prViewState{HeadRepository: prRepository{Name: "repo"}},
			want:  "",
		},
		{
			name:  "missing name yields empty",
			state: prViewState{HeadRepoOwner: prActor{Login: "acme"}},
			want:  "",
		},
	}
	for _, tt := range tests {
		if got := tt.state.HeadRepositoryNameWithOwner(); got != tt.want {
			t.Fatalf("%s: HeadRepositoryNameWithOwner() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// ---- maybeQueueReviewFeedback: rollback on error ----------------------------

func TestMaybeQueueReviewFeedbackRollsBackOnError(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	broker.RecordTaskRunConfig("req-feedback-err", []byte(`{"repo":"acme/repo","baseBranch":"main","prompt":"ship it"}`))
	broker.IngestLog("dispatch status=start request_id=req-feedback-err repo=acme/repo")
	broker.IngestLog("dispatch status=completed request_id=req-feedback-err workspace=/tmp/run branch=agent00-ship pr_url=https://github.com/acme/repo/pull/200")

	reviewBody := "**Negative**\n- please fix the regression in the parser."
	prJSON := fmt.Sprintf(`{
		"state":"OPEN",
		"url":"https://github.com/acme/repo/pull/200",
		"headRefName":"agent00-ship",
		"baseRefName":"main",
		"latestReviews":[{"id":"review-1","author":{"login":"reviewer"},"state":"COMMENTED","submittedAt":"2026-05-30T12:00:00Z","body":%q}],
		"comments":[]
	}`, reviewBody)
	// Each checkTaskPR call issues two gh calls: "pr view" (the PR state
	// object above) followed by the inline review-comments fetch (an empty
	// array), since prState always tries to enrich an OPEN PR with review
	// comments.
	runner := &stubPRMonitorRunner{
		results: []execx.Result{
			{Stdout: prJSON},
			{Stdout: "[]"},
			{Stdout: prJSON},
			{Stdout: "[]"},
		},
	}

	calls := 0
	logs := make(chan string, 4)
	monitor := &PRMergeMonitor{
		Runner: runner,
		Broker: broker,
		Logf: func(format string, args ...any) {
			select {
			case logs <- fmt.Sprintf(format, args...):
			default:
			}
		},
		OnReviewFeedback: func(context.Context, Task, PRReviewFeedback) (string, error) {
			calls++
			return "", errors.New("follow up failed")
		},
	}

	task := broker.Snapshot().Tasks[0]
	monitor.checkTaskPR(context.Background(), task)
	monitor.checkTaskPR(context.Background(), task)

	if calls != 2 {
		t.Fatalf("OnReviewFeedback calls = %d, want 2 (rollback must allow requeueing after an error)", calls)
	}

	select {
	case line := <-logs:
		if !strings.Contains(line, "pr_review_feedback") {
			t.Fatalf("log line = %q, want pr_review_feedback warning", line)
		}
	default:
		t.Fatal("expected a warning log line for the failed OnReviewFeedback call")
	}
}

// ---- shouldMonitorTaskReviewFeedback ----------------------------------------

func TestShouldMonitorTaskReviewFeedback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		task Task
		want bool
	}{
		{"no pr url is never monitored", Task{Status: "completed", PRURL: ""}, false},
		{"review source is excluded", Task{Status: "completed", PRURL: "url", Source: "review"}, false},
		{"code-review workflow is excluded", Task{Status: "completed", PRURL: "url", Workflow: "code-review"}, false},
		{"code_review workflow is excluded", Task{Status: "completed", PRURL: "url", Workflow: "CODE_REVIEW"}, false},
		{"pull request code review workflow is excluded", Task{Status: "completed", PRURL: "url", Workflow: "Pull Request Code Review"}, false},
		{"ordinary workflow is monitored", Task{Status: "completed", PRURL: "url", Workflow: "feature-work"}, true},
	}
	for _, tt := range tests {
		if got := shouldMonitorTaskReviewFeedback(tt.task); got != tt.want {
			t.Fatalf("%s: shouldMonitorTaskReviewFeedback() = %t, want %t", tt.name, got, tt.want)
		}
	}
}

// ---- actionablePRReviewFeedback: skip branches ------------------------------

func TestActionablePRReviewFeedbackSkipsAutomatedAndNonActionableEntries(t *testing.T) {
	t.Parallel()

	feedback := actionablePRReviewFeedback(Task{PRURL: "https://github.com/acme/repo/pull/9"}, prViewState{
		State: "OPEN",
		LatestReviews: []prReviewEntry{
			{ID: "auto", State: "COMMENTED", Body: automatedReviewCycleMarker + " pass=1 total=1 -->"},
			{ID: "blank", State: "COMMENTED", Body: "   "},
			{ID: "real", State: "COMMENTED", Body: "Negative\n- please fix the crash"},
		},
		Comments: []prCommentEntry{
			{ID: "plain", Body: "Nice work overall!"},
		},
		ReviewComments: []prReviewCommentEntry{
			{ID: 1, Path: "a.go", Body: automatedReviewCycleMarker + " pass=1 total=1 -->"},
			{ID: 2, Path: "b.go", Body: "Nice work overall!"},
			{ID: 3, Path: "c.go", Line: 5, Body: "please fix this bug"},
		},
	})

	if len(feedback.Items) != 2 {
		t.Fatalf("len(feedback.Items) = %d, want 2 (automated and non-actionable entries skipped)", len(feedback.Items))
	}
	ids := map[string]bool{}
	for _, item := range feedback.Items {
		ids[item.ID] = true
	}
	if !ids["real"] {
		t.Fatalf("feedback.Items = %#v, want an item from the \"real\" review", feedback.Items)
	}
	if !ids["3"] {
		t.Fatalf("feedback.Items = %#v, want an item from review comment id 3", feedback.Items)
	}
}

// ---- commentBodyNeedsFollowUp / negativeReviewBullets / section headings ---

func TestCommentBodyNeedsFollowUp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		body string
		want bool
	}{
		{"Negative\n- fix the crash", true},
		{"", false},
		{"   ", false},
		{"please help with this", true},
		{"this all looks fine to me", false},
	}
	for _, tt := range tests {
		if got := commentBodyNeedsFollowUp(tt.body); got != tt.want {
			t.Fatalf("commentBodyNeedsFollowUp(%q) = %t, want %t", tt.body, got, tt.want)
		}
	}
}

func TestNegativeReviewBulletsStopsAtPositiveOrSectionHeading(t *testing.T) {
	t.Parallel()

	bodyPositive := "**Negative**\n- bug one\n\n**Positive**\n- good thing"
	if got := negativeReviewBullets(bodyPositive); len(got) != 1 || got[0] != "bug one" {
		t.Fatalf("negativeReviewBullets(positive heading) = %#v, want [\"bug one\"]", got)
	}

	bodyHeading := "**Negative**\n- bug one\n### Next Steps\n- not counted"
	if got := negativeReviewBullets(bodyHeading); len(got) != 1 || got[0] != "bug one" {
		t.Fatalf("negativeReviewBullets(markdown heading) = %#v, want [\"bug one\"]", got)
	}

	bodyNone := "no sections here, just plain text"
	if got := negativeReviewBullets(bodyNone); len(got) != 0 {
		t.Fatalf("negativeReviewBullets(no sections) = %#v, want empty", got)
	}
}

func TestIsReviewSectionHeading(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line string
		want bool
	}{
		{"", false},
		{"# Heading", true},
		{"**Bold**", true},
		{"plain text", false},
		{"**not closed", false},
	}
	for _, tt := range tests {
		if got := isReviewSectionHeading(tt.line); got != tt.want {
			t.Fatalf("isReviewSectionHeading(%q) = %t, want %t", tt.line, got, tt.want)
		}
	}
}

func TestTrimFeedbackBulletReturnsEmptyWithoutBulletPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line string
		want string
	}{
		{"- item", "item"},
		{"* item", "item"},
		{"• item", "item"},
		{"no bullet prefix", ""},
	}
	for _, tt := range tests {
		if got := trimFeedbackBullet(tt.line); got != tt.want {
			t.Fatalf("trimFeedbackBullet(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

// ---- reviewCommentLocation / truncateFeedbackText --------------------------

func TestReviewCommentLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		comment prReviewCommentEntry
		want    string
	}{
		{"blank path yields empty", prReviewCommentEntry{Path: "", Line: 5}, ""},
		{"line present", prReviewCommentEntry{Path: "a.go", Line: 5}, "a.go:5"},
		{"falls back to original line", prReviewCommentEntry{Path: "a.go", Line: 0, OriginalLine: 9}, "a.go:9"},
		{"no usable line yields bare path", prReviewCommentEntry{Path: "a.go", Line: 0, OriginalLine: 0}, "a.go"},
	}
	for _, tt := range tests {
		if got := reviewCommentLocation(tt.comment); got != tt.want {
			t.Fatalf("%s: reviewCommentLocation() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestTruncateFeedbackTextTruncatesLongBodies(t *testing.T) {
	t.Parallel()

	short := "short feedback"
	if got := truncateFeedbackText(short); got != short {
		t.Fatalf("truncateFeedbackText(short) = %q, want unchanged", got)
	}

	long := strings.Repeat("a", 6500)
	got := truncateFeedbackText(long)
	if !strings.HasSuffix(got, "[truncated]") {
		t.Fatalf("truncateFeedbackText(long) = %q, want suffix [truncated]", got)
	}
	if len(got) >= len(long) {
		t.Fatalf("len(truncateFeedbackText(long)) = %d, want shorter than input (%d)", len(got), len(long))
	}
}

// ---- prState / prReviewComments --------------------------------------------

func TestPRStateRequiresNonBlankURL(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{Runner: &stubPRMonitorRunner{}}
	_, err := m.prState(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "pull request url is required") {
		t.Fatalf("prState(blank url) error = %v, want pull request url is required", err)
	}
}

func TestPRStateReturnsDecodeErrorForMalformedJSON(t *testing.T) {
	t.Parallel()

	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: "not-json"}}
	m := &PRMergeMonitor{Runner: runner}
	_, err := m.prState(context.Background(), "https://github.com/acme/repo/pull/1")
	if err == nil || !strings.Contains(err.Error(), "decode gh pr view response") {
		t.Fatalf("prState(malformed json) error = %v, want decode gh pr view response", err)
	}
}

func TestPRStateLogsWarnWhenReviewCommentsFetchFails(t *testing.T) {
	t.Parallel()

	runner := &stubPRMonitorRunner{
		results: []execx.Result{{Stdout: `{"state":"OPEN"}`}, {}},
		errs:    []error{nil, errors.New("boom")},
	}
	logs := make(chan string, 4)
	m := &PRMergeMonitor{
		Runner: runner,
		Logf: func(format string, args ...any) {
			select {
			case logs <- fmt.Sprintf(format, args...):
			default:
			}
		},
	}

	state, err := m.prState(context.Background(), "https://github.com/acme/repo/pull/1")
	if err != nil {
		t.Fatalf("prState() error = %v, want nil", err)
	}
	if state.ReviewComments != nil {
		t.Fatalf("state.ReviewComments = %#v, want nil when the comments fetch fails", state.ReviewComments)
	}
	select {
	case line := <-logs:
		if !strings.Contains(line, "pr_monitor_review_comments") {
			t.Fatalf("log line = %q, want pr_monitor_review_comments warning", line)
		}
	default:
		t.Fatal("expected a warning log line for the failed review comments fetch")
	}
}

func TestPRReviewCommentsHandlesNonPRURLRunErrorsAndEmptyPayloads(t *testing.T) {
	t.Parallel()

	m := &PRMergeMonitor{Runner: &stubPRMonitorRunner{}}
	comments, err := m.prReviewComments(context.Background(), "not-a-pr-url")
	if err != nil || comments != nil {
		t.Fatalf("prReviewComments(non pr url) = (%#v, %v), want (nil, nil)", comments, err)
	}

	errRunner := &stubPRMonitorRunner{err: errors.New("boom")}
	mErr := &PRMergeMonitor{Runner: errRunner}
	if _, err := mErr.prReviewComments(context.Background(), "https://github.com/acme/repo/pull/1"); err == nil {
		t.Fatal("prReviewComments(runner error) error = nil, want non-nil")
	}

	blankRunner := &stubPRMonitorRunner{result: execx.Result{Stdout: "   "}}
	mBlank := &PRMergeMonitor{Runner: blankRunner}
	comments, err = mBlank.prReviewComments(context.Background(), "https://github.com/acme/repo/pull/1")
	if err != nil || comments != nil {
		t.Fatalf("prReviewComments(blank stdout) = (%#v, %v), want (nil, nil)", comments, err)
	}

	badJSONRunner := &stubPRMonitorRunner{result: execx.Result{Stdout: "not-json"}}
	mBadJSON := &PRMergeMonitor{Runner: badJSONRunner}
	if _, err := mBadJSON.prReviewComments(context.Background(), "https://github.com/acme/repo/pull/1"); err == nil || !strings.Contains(err.Error(), "decode pull request review comments response") {
		t.Fatalf("prReviewComments(malformed json) error = %v, want decode pull request review comments response", err)
	}
}
