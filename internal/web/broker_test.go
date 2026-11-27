package web

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWorkflowTransitionRemainsAnEventWithoutCreatingATask(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog(`workflow event=transition workflow_id="delivery" run_id="run-1" request_id="req-42" status=waiting reason=approval_pending`)
	snap := b.Snapshot()
	if len(snap.Tasks) != 0 {
		t.Fatalf("workflow transition created a task: %+v", snap.Tasks)
	}
	if len(snap.Events) != 1 || snap.Events[0].RequestID != "req-42" {
		t.Fatalf("workflow transition lost its event or request ID: %+v", snap.Events)
	}
}

func TestDashboardCountsWorkflowDurationOnce(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	const workflow = " workflow_id=delivery run_id=run-1 request_id=req-42 started_at=2026-01-01T00:00:00Z"
	b.IngestLog("workflow event=submission" + workflow + " status=running duration_ms=unknown")
	now = now.Add(10 * time.Second)
	b.IngestLog("task status=start request_id=req-42")
	now = now.Add(20 * time.Second)
	b.IngestLog("task status=completed request_id=req-42")
	if got := b.Snapshot().Stats.TotalSavedSeconds; got != 0 {
		t.Fatalf("task time saved before workflow completion = %v, want 0", got)
	}
	b.IngestLog("workflow event=transition" + workflow + " status=waiting reason=checks_pending duration_ms=unknown")
	if got := b.Snapshot().Stats.TotalSavedSeconds; got != 0 {
		t.Fatalf("waiting workflow time saved = %v, want 0", got)
	}
	now = now.Add(50 * time.Second)
	merge := "workflow event=transition" + workflow + " status=merged duration_ms=80000"
	b.IngestLog(merge)
	b.IngestLog("workflow event=transition workflow_id=delivery request_id=restored status=merged duration_ms=30000")
	b.IngestLog("task status=start request_id=standalone")
	now = now.Add(15 * time.Second)
	b.IngestLog("task status=completed request_id=standalone")
	b.IngestLog(merge)
	if got := b.Snapshot().Stats.TotalSavedSeconds; got != 125 {
		t.Fatalf("time saved = %v, want 125", got)
	}
}

func TestDashboardWorkflowTimeSavedIncludesRetryAndConfirmedExternalMerge(t *testing.T) {
	t.Parallel()

	for _, requestID := range []string{"original", "retry"} {
		t.Run(requestID, func(t *testing.T) {
			t.Parallel()
			b := NewBroker()
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			b.now = func() time.Time { return now }
			const original = " workflow_id=delivery run_id=run-1 request_id=original started_at=2026-01-01T00:00:00Z"
			b.IngestLog("workflow event=submission" + original + " status=running duration_ms=unknown")
			b.IngestLog("task status=start request_id=original")
			now = now.Add(time.Minute)
			b.IngestLog("task status=completed request_id=original")
			b.IngestLog("workflow event=transition" + original + " status=completed reason=completed duration_ms=60000")
			if got := b.Snapshot().Stats.TotalSavedSeconds; got != 0 {
				t.Fatalf("workflow closed without merging = %v seconds saved, want 0", got)
			}
			now = now.Add(20 * time.Second)
			if requestID != "original" {
				b.RecordTaskRerunAttempt("original", requestID)
			}
			retry := " workflow_id=delivery run_id=run-2 request_id=" + requestID + " started_at=2026-01-01T00:01:20Z"
			b.IngestLog("workflow event=submission" + retry + " status=running duration_ms=unknown")
			b.IngestLog("task status=start request_id=" + requestID)
			now = now.Add(10 * time.Second)
			b.IngestLog("task status=completed request_id=" + requestID)
			b.IngestLog("workflow event=transition" + retry + " status=completed reason=merge_unconfirmed duration_ms=10000")
			if stats := b.Snapshot().Stats; stats.TotalSavedSeconds != 90 || stats.TotalTasks != 1 {
				t.Fatalf("retry stats = %+v, want 90 seconds saved for one task", stats)
			}
			if err := b.ForgetTask(requestID); err != nil {
				t.Fatal(err)
			}
			if got := b.Snapshot().Stats.TotalSavedSeconds; got != 0 {
				t.Fatalf("time saved after forgetting task = %v, want 0", got)
			}
		})
	}
}

func TestBrokerShowsCheckWaitOnExistingTask(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	const id = "issue-checks"
	b.IngestLog("task status=start request_id=" + id + " explicit_terminal=true")
	b.IngestLog("task status=queued request_id=" + id + " action=wait_for_github_checks pr_url=https://github.com/acme/repo/pull/42")
	task, ok := b.Task(id)
	if !ok || task.Status != "pending" || task.Stage != "checks" || task.StageStatus != "waiting" ||
		task.Warning == "" || task.Error != "" || task.PRURL != "https://github.com/acme/repo/pull/42" {
		t.Fatalf("check wait was not visible on the existing task: %+v", task)
	}
	b.IngestLog("task status=queued request_id=" + id + " action=resume_draft_verification")
	b.IngestLog("task status=start request_id=" + id + " explicit_terminal=true")
	b.IngestLog("task status=completed request_id=" + id)
	tasks := b.TaskList()
	if len(tasks) != 1 || tasks[0].RequestID != id || tasks[0].Status != "completed" || tasks[0].Warning != "" {
		t.Fatalf("check recovery created another task or kept stale status: %+v", tasks)
	}
}

func TestBrokerTracksTaskLifecycleAndCommandOutput(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-42"

	b.IngestLog("task status=start request_id=req-42 skill=code_for_me repo=git@github.com:acme/repo.git repos=git@github.com:acme/repo.git,git@github.com:acme/repo-two.git")
	b.IngestLog("task request_id=req-42 stage=codex status=start")
	b.IngestLog("task request_id=req-42 cmd phase=codex name=codex stream=stdout b64=" + base64.StdEncoding.EncodeToString([]byte("thinking...")))
	b.IngestLog("task status=completed request_id=req-42 workspace=/tmp/run branch=agent00-feature pr_url=https://github.com/acme/repo/pull/99")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.RequestID != requestID {
		t.Fatalf("task.RequestID = %q", task.RequestID)
	}
	if task.Status != "completed" {
		t.Fatalf("task.Status = %q, want completed", task.Status)
	}
	if task.Stage != "codex" {
		t.Fatalf("task.Stage = %q, want codex", task.Stage)
	}
	if got, want := len(task.Repos), 2; got != want {
		t.Fatalf("len(task.Repos) = %d, want %d", got, want)
	}
	if task.Repos[0] != "git@github.com:acme/repo.git" || task.Repos[1] != "git@github.com:acme/repo-two.git" {
		t.Fatalf("task.Repos = %#v", task.Repos)
	}
	if task.PRURL != "https://github.com/acme/repo/pull/99" {
		t.Fatalf("task.PRURL = %q", task.PRURL)
	}

	foundOutput := false
	for _, line := range task.Logs {
		if line.Stream == "stdout" && line.Text == "thinking..." {
			foundOutput = true
			break
		}
	}
	if !foundOutput {
		t.Fatalf("task logs missing decoded stdout line: %#v", task.Logs)
	}
}

func TestBrokerTaskStatusRevisionChangesOnlyWithStatus(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	const requestID = "req-status-revision"
	const requestField = " request_id=" + requestID
	for _, step := range []struct {
		line     string
		status   string
		revision uint64
	}{
		{"task status=start" + requestField, "running", 1},
		{"task" + requestField + " stage=codex status=start", "running", 1},
		{"task status=paused" + requestField, "paused", 2},
		{"task status=start" + requestField, "running", 3},
		{"task status=completed" + requestField, "completed", 4},
	} {
		b.IngestLog(step.line)
		tasks := b.Snapshot().Tasks
		if len(tasks) != 1 || tasks[0].Status != step.status || tasks[0].StatusRevision != step.revision {
			t.Fatalf("after %q, tasks = %+v, want %s at revision %d", step.line, tasks, step.status, step.revision)
		}
	}
	for range 2 {
		if err := b.MarkTaskPRMerged(requestID, ""); err != nil {
			t.Fatal(err)
		}
		task, ok := b.Task(requestID)
		if !ok || task.Status != "merged" || task.StatusRevision != 5 {
			t.Fatalf("after merge, task = %+v, want merged at revision 5", task)
		}
	}
}

func TestBrokerPreservesStatusTransitionsBetweenSnapshots(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	b.IngestLog("task status=start request_id=transition-fixture")
	before := b.Snapshot().Tasks[0]
	b.IngestLog("task status=paused request_id=transition-fixture")
	b.IngestLog("task status=start request_id=transition-fixture")
	after := b.StreamSnapshot().Tasks[0]
	if len(before.StatusTransitions) != 1 || len(after.StatusTransitions) != 3 {
		t.Fatalf("history lengths before=%d after=%d", len(before.StatusTransitions), len(after.StatusTransitions))
	}
	for i, want := range []string{"running", "paused", "running"} {
		if got := after.StatusTransitions[i]; got.Status != want || got.Revision != uint64(i+1) {
			t.Fatalf("transition %d = %+v", i, got)
		}
	}
	after.StatusTransitions[0].Status = "mutated"
	task, _ := b.Task("transition-fixture")
	if task.StatusTransitions[0].Status != "running" {
		t.Fatal("snapshot mutated broker history")
	}
	for range 130 {
		b.IngestLog("task status=paused request_id=transition-fixture")
		b.IngestLog("task status=start request_id=transition-fixture")
	}
	task, _ = b.Task("transition-fixture")
	if len(task.StatusTransitions) != 128 || task.StatusTransitions[127].Revision != task.StatusRevision {
		t.Fatal("history must retain the latest 128 transitions")
	}
}

func TestBrokerRecordsTaskPRTitlesByURL(t *testing.T) {
	t.Parallel()

	const requestID = "req-title"
	const primaryURL = "https://github.com/acme/repo/pull/99"
	const otherURL = "https://github.com/acme/repo-two/pull/42"
	const runConfig = `{"prompt":"Original prompt","githubIssue":{"title":"Issue title"},"prTitle":"Configured title"}`
	b := NewBroker()
	b.RecordTaskRunConfig(requestID, []byte(runConfig))
	if task, _ := b.Task(requestID); task.IssueTitle != "Issue title" || task.PRTitle != "" {
		t.Fatalf("task before creation = %+v", task)
	}

	b.RecordTaskPRTitle(requestID, primaryURL, "First title")
	if task, _ := b.Task(requestID); task.PRTitle != "First title" || task.PRURL != primaryURL {
		t.Fatalf("task after creation = %+v", task)
	}

	b.RecordTaskPRTitle(requestID, primaryURL, "Updated title")
	b.RecordTaskPRTitle(requestID, otherURL, "Other title")
	b.IngestLog("task status=completed request_id=" + requestID + " pr_url=" + primaryURL)
	if task := b.StreamSnapshot().Tasks[0]; task.IssueTitle != "Issue title" || task.PRTitle != "Updated title" || task.PRTitleURL != primaryURL || task.Prompt != "Original prompt" {
		t.Fatalf("completed task = %+v", task)
	}
	if raw, ok := b.TaskRunConfig(requestID); !ok || string(raw) != runConfig {
		t.Fatalf("rerun config = %q, want %s", raw, runConfig)
	}
}

func TestBrokerKeepsCreatedPRTitleWhenCompletionSelectsReusedPR(t *testing.T) {
	t.Parallel()

	const requestID = "req-multi-repo"
	const reusedURL = "https://github.com/acme/repo/pull/99"
	const createdURL = "https://github.com/acme/repo-two/pull/42"
	const createdTitle = "Title of created PR"
	b := NewBroker()
	b.IngestLog("task status=start request_id=" + requestID)
	b.RecordCreatedTaskPRTitle(requestID, createdURL, createdTitle)
	b.RecordTaskPRTitle(requestID, reusedURL, "Reused PR title")
	if task, _ := b.Task(requestID); task.PRTitleURL != createdURL || task.PRTitle != createdTitle {
		t.Fatalf("in-progress task lost created PR metadata: %+v", task)
	}
	b.IngestLog("task status=completed request_id=" + requestID + " pr_url=" + reusedURL + " pr_urls=" + reusedURL + "," + createdURL)
	b.recordTaskPRTitle(requestID, reusedURL, "Monitored reused PR title", taskPRTitleMonitored)

	task, _ := b.Task(requestID)
	for _, task := range []Task{task, b.StreamSnapshot().Tasks[0]} {
		if task.PRURL != reusedURL || task.PRTitleURL != createdURL || task.PRTitle != createdTitle || task.Status != "completed" {
			t.Errorf("completed task = %+v, want primary URL %q and title %q paired with %q", task, reusedURL, createdTitle, createdURL)
		}
	}
}

func TestBrokerDropsSupersededPRTitleWhenReplacementHasNoTitle(t *testing.T) {
	t.Parallel()

	const requestID = "req-replacement"
	const closedURL = "https://github.com/acme/repo/pull/99"
	const replacementURL = "https://github.com/acme/repo/pull/100"
	const otherURL = "https://github.com/acme/other/pull/42"
	const otherTitle = "Title from another repository"
	for _, tc := range []struct {
		name, fallbackURL, fallbackTitle string
	}{
		{name: "single repo"},
		{name: "multi repo", fallbackURL: otherURL, fallbackTitle: otherTitle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := NewBroker()
			b.IngestLog("task status=start request_id=" + requestID)
			b.RecordTaskPRTitle(requestID, tc.fallbackURL, tc.fallbackTitle)
			b.RecordTaskPRTitle(requestID, closedURL, "Closed PR title")
			b.IngestLog("task request_id=" + requestID + " stage=pr status=ok action=recreate_closed_pr old_pr_url=" + closedURL + " pr_url=" + replacementURL)

			check := func(task Task) {
				t.Helper()
				if task.PRURL != replacementURL || task.PRTitle != tc.fallbackTitle || task.PRTitleURL != tc.fallbackURL {
					t.Fatalf("PR metadata = (%q, %q, %q), want (%q, %q, %q)",
						task.PRURL, task.PRTitle, task.PRTitleURL, replacementURL, tc.fallbackTitle, tc.fallbackURL)
				}
			}
			task, _ := b.Task(requestID)
			check(task)
			b.IngestLog("task status=completed request_id=" + requestID + " pr_url=" + replacementURL)
			check(b.StreamSnapshot().Tasks[0])
		})
	}
}

func TestBrokerTracksLiveAppURLFromTaskLogs(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-live skill=code_for_me repo=git@github.com:acme/app.git")
	b.IngestLog("task request_id=req-live event=live_app_ready live_app_url=http://127.0.0.1:5173")
	b.IngestLog("task status=completed request_id=req-live workspace=/tmp/run branch=agent00-app")

	task, ok := b.Task("req-live")
	if !ok {
		t.Fatal("Task(req-live) ok = false, want true")
	}
	if got, want := task.LiveAppURL, "http://127.0.0.1:5173"; got != want {
		t.Fatalf("task.LiveAppURL = %q, want %q", got, want)
	}

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got, want := snap.Tasks[0].LiveAppURL, "http://127.0.0.1:5173"; got != want {
		t.Fatalf("snapshot task.LiveAppURL = %q, want %q", got, want)
	}
}

func TestBrokerIncludesPromptImagesInTaskSnapshots(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-images", []byte(`{
		"repo":"git@github.com:acme/repo.git",
		"prompt":"inspect screenshot",
		"images":[
			{"name":" shot.png ","mediaType":" image/png ","dataBase64":" ZGF0YQ== "},
			{"name":"blank.png","mediaType":"image/png","dataBase64":" "}
		]
	}`))

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	images := snap.Tasks[0].Images
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
	if got, want := images[0].Name, "shot.png"; got != want {
		t.Fatalf("images[0].Name = %q, want %q", got, want)
	}
	if got, want := images[0].MediaType, "image/png"; got != want {
		t.Fatalf("images[0].MediaType = %q, want %q", got, want)
	}
	if got, want := images[0].DataBase64, "ZGF0YQ=="; got != want {
		t.Fatalf("images[0].DataBase64 = %q, want %q", got, want)
	}

	task, ok := b.Task("req-images")
	if !ok {
		t.Fatal("Task(req-images) ok = false, want true")
	}
	task.Images[0].Name = "mutated.png"
	next, _ := b.Task("req-images")
	if got, want := next.Images[0].Name, "shot.png"; got != want {
		t.Fatalf("Task returned mutable images slice, got %q want %q", got, want)
	}
}

func TestBrokerTracksTaskRuntimeAndSavedTimeStats(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.sessionStartedAt = now.Add(-5 * time.Minute)
	b.now = func() time.Time { return now }

	b.RecordTaskRunConfig("req-1", []byte(`{"repo":"git@github.com:acme/repo.git","libraryTaskName":"unit-test-coverage","agentHarness":"codex"}`))
	b.IngestLog("task status=start request_id=req-1 skill=library_task repo=git@github.com:acme/repo.git")
	b.IngestLog("task request_id=req-1 stage=codex status=start")
	now = now.Add(2 * time.Minute)
	b.IngestLog("task status=completed request_id=req-1 workspace=/tmp/run branch=agent00-feature")

	b.RecordTaskRunConfig("req-2", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"fix tests","agentHarness":"claude"}`))
	now = now.Add(time.Minute)
	b.IngestLog("task status=start request_id=req-2 skill=code_for_me repo=git@github.com:acme/repo.git")
	now = now.Add(time.Minute)

	snap := b.Snapshot()
	if got, want := snap.Stats.SessionRuntimeSeconds, 540.0; got != want {
		t.Fatalf("stats.session_runtime_seconds = %v, want %v", got, want)
	}
	if got, want := snap.Stats.TotalSavedSeconds, 120.0; got != want {
		t.Fatalf("stats.total_saved_seconds = %v, want %v", got, want)
	}
	if got, want := snap.Stats.AverageDurationSeconds, 120.0; got != want {
		t.Fatalf("stats.average_duration_seconds = %v, want %v", got, want)
	}
	if len(snap.Stats.WorkflowTimes) == 0 || snap.Stats.WorkflowTimes[0].Name != "100% Unit Test Coverage" {
		t.Fatalf("stats.workflow_times = %#v, want 100%% Unit Test Coverage first", snap.Stats.WorkflowTimes)
	}
	if got, want := snap.Stats.WorkflowTimes[0].TotalSavedSeconds, 120.0; got != want {
		t.Fatalf("workflow total_saved_seconds = %v, want %v", got, want)
	}
	if len(snap.Stats.AgentTimes) == 0 || snap.Stats.AgentTimes[0].Name != "codex" {
		t.Fatalf("stats.agent_times = %#v, want codex first", snap.Stats.AgentTimes)
	}
	var active Task
	for _, task := range snap.Tasks {
		if task.RequestID == "req-2" {
			active = task
			break
		}
	}
	if active.AgentHarness != "claude" {
		t.Fatalf("active.AgentHarness = %q, want claude", active.AgentHarness)
	}
	if active.DurationSeconds != 120 {
		t.Fatalf("active.DurationSeconds = %v, want 120", active.DurationSeconds)
	}
}

func TestBrokerTracksDashboardSourceMix(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfigWithSource("local-chat", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"from chat"}`), "chat")
	b.RecordTaskRunConfigWithSource("local-json", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"from json"}`), "json")
	b.RecordTaskRunConfigWithSource("local-library", []byte(`{"repo":"git@github.com:acme/repo.git","libraryTaskName":"unit-test-coverage"}`), "library")
	b.RecordTaskRunConfigWithSource("local-prompt", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"from prompt"}`), "prompt")
	b.RecordTaskRunConfigWithSource("local-review", []byte(`{"repo":"git@github.com:acme/repo.git","libraryTaskName":"code-review"}`), "review")
	b.RecordTaskRunConfigWithSource("local-github-issues", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"from GitHub Issues"}`), "github_issues")
	b.RecordTaskRunConfig("local-issue-metadata", []byte(`{"repoUrl":"https://github.com/acme/repo","prompt":"from issue metadata","githubIssue":{"number":42,"repository":"acme/repo","url":"https://github.com/acme/repo/issues/42"}}`))
	b.RecordTaskRunConfigWithSource("unknown-source", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"from unknown source"}`), "unknown")
	b.IngestLog("task status=start request_id=issue-restored")

	snap := b.Snapshot()
	got := map[string]int{}
	for _, group := range snap.Stats.SourceMix {
		got[group.Name] = group.Tasks
	}
	want := map[string]int{"Chat": 1, "GitHub Issues": 3, "JSON": 1, "Library": 2, "Prompt": 2}
	if len(got) != len(want) {
		t.Fatalf("source_mix = %v, want %v", got, want)
	}
	for name, count := range want {
		if got[name] != count {
			t.Fatalf("source_mix[%q] = %d, want %d", name, got[name], count)
		}
	}
}

func TestBrokerTracksReviewRuntimeAsDashboardSavedTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }

	b.RecordTaskRunConfigWithSource("req-hub-review", []byte(`{
		"repo":"git@github.com:acme/repo.git",
		"libraryTaskName":"code-review",
		"agentHarness":"codex",
		"review":{"prNumber":42}
	}`), "unknown")
	b.IngestLog("task status=start request_id=req-hub-review skill=library_task repo=git@github.com:acme/repo.git")
	now = now.Add(2 * time.Minute)
	b.IngestLog("task status=no_changes request_id=req-hub-review workspace=/tmp/review branch=feature pr_url=https://github.com/acme/repo/pull/42")

	b.RecordTaskRunConfigWithSource("local-review", []byte(`{
		"repo":"git@github.com:acme/repo.git",
		"libraryTaskName":"code-review",
		"agentHarness":"codex",
		"review":{"prNumber":43}
	}`), "review")
	now = now.Add(30 * time.Second)

	snap := b.Snapshot()
	if got, want := snap.Stats.TotalTasks, 2; got != want {
		t.Fatalf("stats.total_tasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.TotalSavedSeconds, 150.0; got != want {
		t.Fatalf("stats.total_saved_seconds = %v, want %v", got, want)
	}
	if got, want := snap.Stats.ReviewTasks, 2; got != want {
		t.Fatalf("stats.review_tasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.ReviewCompletedTasks, 1; got != want {
		t.Fatalf("stats.review_completed_tasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.ReviewActiveTasks, 1; got != want {
		t.Fatalf("stats.review_active_tasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.ReviewSavedSeconds, 150.0; got != want {
		t.Fatalf("stats.review_saved_seconds = %v, want %v", got, want)
	}
	if len(snap.Stats.WorkflowTimes) == 0 || snap.Stats.WorkflowTimes[0].Name != "Pull Request Code Review" {
		t.Fatalf("stats.workflow_times = %#v, want Pull Request Code Review first", snap.Stats.WorkflowTimes)
	}
	if got, want := snap.Stats.WorkflowTimes[0].TotalSavedSeconds, 150.0; got != want {
		t.Fatalf("review workflow total_saved_seconds = %v, want %v", got, want)
	}
}

func TestBrokerCountsFailedReviewRuntimeAsSavedTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }

	b.RecordTaskRunConfigWithSource("req-review-error", []byte(`{
		"repo":"git@github.com:acme/repo.git",
		"libraryTaskName":"code-review",
		"agentHarness":"codex",
		"review":{"prNumber":42}
	}`), "review")
	b.IngestLog("task status=start request_id=req-review-error skill=library_task repo=git@github.com:acme/repo.git")
	now = now.Add(45 * time.Second)
	b.IngestLog("task status=error request_id=req-review-error error=blocked")

	snap := b.Snapshot()
	if got, want := snap.Stats.TotalSavedSeconds, 45.0; got != want {
		t.Fatalf("stats.total_saved_seconds = %v, want %v", got, want)
	}
	if got, want := snap.Stats.ReviewSavedSeconds, 45.0; got != want {
		t.Fatalf("stats.review_saved_seconds = %v, want %v", got, want)
	}
	if got, want := snap.Stats.ReviewFailedTasks, 1; got != want {
		t.Fatalf("stats.review_failed_tasks = %d, want %d", got, want)
	}
	if len(snap.Stats.WorkflowTimes) == 0 || snap.Stats.WorkflowTimes[0].Name != "Pull Request Code Review" {
		t.Fatalf("stats.workflow_times = %#v, want Pull Request Code Review first", snap.Stats.WorkflowTimes)
	}
	if got, want := snap.Stats.WorkflowTimes[0].TotalSavedSeconds, 45.0; got != want {
		t.Fatalf("review workflow total_saved_seconds = %v, want %v", got, want)
	}
}

func TestBrokerTracksRepositoryStatsAndPullRequests(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }
	b.SetGitHubRepositories([]GitHubRepo{{
		Name:           "repo",
		FullName:       "acme/repo",
		HTMLURL:        "https://github.com/acme/repo",
		OwnerType:      "Organization",
		OwnerAvatarURL: "https://avatars.githubusercontent.com/u/42?v=4",
		DefaultBranch:  "main",
		Private:        true,
	}})

	b.RecordTaskRunConfig("req-1", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"ship it"}`))
	b.IngestLog("task status=start request_id=req-1 repo=git@github.com:acme/repo.git")
	now = now.Add(2 * time.Minute)
	b.IngestLog("task status=completed request_id=req-1 pr_url=https://github.com/acme/repo/pull/1")
	b.RecordTaskRunConfig("req-2", []byte(`{"repo":"https://github.com/acme/new-repo","prompt":"keep going"}`))

	snap := b.Snapshot()
	if len(snap.Repositories) != 2 {
		t.Fatalf("repositories = %#v, want loaded repo plus runtime repo", snap.Repositories)
	}
	var loaded Repository
	for _, repo := range snap.Repositories {
		if repo.Key == "acme/repo" {
			loaded = repo
			break
		}
	}
	if loaded.Key == "" {
		t.Fatalf("repositories = %#v, want acme/repo", snap.Repositories)
	}
	if !loaded.Private || loaded.Public || !loaded.Organization || loaded.Personal || loaded.Visibility != RepositoryVisibilityPrivate || loaded.OwnerKind != RepositoryOwnerOrganization {
		t.Fatalf("loaded repository flags = %#v", loaded)
	}
	if got, want := loaded.OwnerAvatarURL, "https://avatars.githubusercontent.com/u/42?v=4"; got != want {
		t.Fatalf("loaded.OwnerAvatarURL = %q, want %q", got, want)
	}
	if got, want := loaded.Stats.CompletedTasks, 1; got != want {
		t.Fatalf("loaded.Stats.CompletedTasks = %d, want %d", got, want)
	}
	if got, want := loaded.Stats.TotalSavedSeconds, 120.0; got != want {
		t.Fatalf("loaded.Stats.TotalSavedSeconds = %v, want %v", got, want)
	}
	if got, want := strings.Join(loaded.PullRequests, ","), "https://github.com/acme/repo/pull/1"; got != want {
		t.Fatalf("loaded.PullRequests = %q, want %q", got, want)
	}
	if snap.Stats.TotalTasks != 2 || snap.Stats.CompletedTasks != 1 || snap.Stats.ActiveTasks != 1 {
		t.Fatalf("dashboard stats = %#v, want deduped repository task totals", snap.Stats)
	}
}

func TestBrokerDedupesDashboardStatsAcrossMultiRepoTasks(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-multi", []byte(`{"repos":["git@github.com:acme/repo.git","git@github.com:acme/repo-two.git"],"prompt":"run everywhere"}`))

	snap := b.Snapshot()
	if got, want := snap.Stats.TotalTasks, 1; got != want {
		t.Fatalf("stats.total_tasks = %d, want %d", got, want)
	}
	if len(snap.Repositories) != 2 {
		t.Fatalf("repositories = %#v, want both repos tracked", snap.Repositories)
	}
	for _, repo := range snap.Repositories {
		if repo.Stats.TotalTasks != 1 {
			t.Fatalf("repo %s stats = %#v, want task counted for each repo", repo.Key, repo.Stats)
		}
	}
}

func TestBrokerMergesRuntimeAndLoadedRepositoryMetadata(t *testing.T) {
	t.Parallel()

	var nilBroker *Broker
	nilBroker.SetGitHubRepositories([]GitHubRepo{{Name: "ignored"}})

	b := NewBroker()
	b.RecordTaskRunConfig("req-runtime", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"before preload"}`))
	b.SetGitHubRepositories([]GitHubRepo{{
		Name:           "repo",
		FullName:       "acme/repo",
		Description:    "Docs",
		HTMLURL:        "https://github.com/acme/repo",
		OwnerType:      "User",
		OwnerAvatarURL: "https://avatars.githubusercontent.com/u/100?v=4",
		DefaultBranch:  "trunk",
		Private:        false,
		Language:       "Go",
	}})

	snap := b.Snapshot()
	if len(snap.Repositories) != 1 {
		t.Fatalf("repositories = %#v, want merged single repo", snap.Repositories)
	}
	repo := snap.Repositories[0]
	if repo.FullName != "acme/repo" || repo.Description != "Docs" || repo.OwnerAvatarURL != "https://avatars.githubusercontent.com/u/100?v=4" || repo.DefaultBranch != "trunk" || repo.Language != "Go" {
		t.Fatalf("repository metadata = %#v, want loaded metadata merged onto runtime repo", repo)
	}
	if !repo.Public || repo.Private || !repo.Personal || repo.Organization || repo.Visibility != RepositoryVisibilityPublic || repo.OwnerKind != RepositoryOwnerPersonal {
		t.Fatalf("repository flags = %#v, want public personal repository", repo)
	}
	if repo.Stats.TotalTasks != 1 {
		t.Fatalf("repository stats = %#v, want existing runtime task preserved", repo.Stats)
	}
}

func TestRepositoryKeyNormalizesGitHubReferences(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"git@github.com:Acme/Repo.git":      "acme/repo",
		"https://github.com/Acme/Repo.git":  "acme/repo",
		"http://github.com/Acme/Repo":       "acme/repo",
		"Acme/Repo":                         "acme/repo",
		"https://example.com/Acme/Repo.git": "https://example.com/acme/repo",
		"":                                  "",
	}
	for input, want := range cases {
		if got := repositoryKey(input); got != want {
			t.Fatalf("repositoryKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMergeRepositoryKeepsExistingClassificationFallbacks(t *testing.T) {
	t.Parallel()

	merged := mergeRepository(Repository{
		Key:          "acme/repo",
		Visibility:   RepositoryVisibilityPrivate,
		OwnerKind:    RepositoryOwnerOrganization,
		PullRequests: []string{"https://github.com/acme/repo/pull/1"},
		Stats:        RepositoryStats{TotalTasks: 1},
	}, Repository{Key: "acme/repo"})
	if !merged.Private || merged.Public || !merged.Organization || merged.Personal {
		t.Fatalf("merged flags = %#v, want existing private organization fallback", merged)
	}
	if merged.Visibility != RepositoryVisibilityPrivate || merged.OwnerKind != RepositoryOwnerOrganization {
		t.Fatalf("merged classification = %#v", merged)
	}
}

func TestBrokerRecordsMergedPRReleaseFromTask(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }

	b.RecordTaskRunConfig("req-release", []byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"main","prompt":"ship release panel","agentHarness":"codex"}`))
	b.IngestLog("task status=start request_id=req-release repo=git@github.com:acme/repo.git")
	now = now.Add(3 * time.Minute)
	b.IngestLog("task status=completed request_id=req-release workspace=/tmp/run branch=agent00-release pr_url=https://github.com/acme/repo/pull/42")

	task, ok := b.Task("req-release")
	if !ok {
		t.Fatal("Task(req-release) found = false, want true")
	}
	now = now.Add(time.Minute)
	b.RecordReleaseFromTask(task, "2026-05-07T12:04:00Z")

	snap := b.Snapshot()
	if got, want := len(snap.Releases), 1; got != want {
		t.Fatalf("len(releases) = %d, want %d", got, want)
	}
	release := snap.Releases[0]
	if got, want := release.RequestID, "req-release"; got != want {
		t.Fatalf("release.RequestID = %q, want %q", got, want)
	}
	if got, want := release.Prompt, "ship release panel"; got != want {
		t.Fatalf("release.Prompt = %q, want %q", got, want)
	}
	if got, want := release.PRURL, "https://github.com/acme/repo/pull/42"; got != want {
		t.Fatalf("release.PRURL = %q, want %q", got, want)
	}
	if got, want := release.DurationSeconds, 180.0; got != want {
		t.Fatalf("release.DurationSeconds = %v, want %v", got, want)
	}
	if got, want := release.MergedAt, "2026-05-07T12:04:00Z"; got != want {
		t.Fatalf("release.MergedAt = %q, want %q", got, want)
	}
}

func TestBrokerCountsMergedTasksAsCompletedNotActive(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-merged", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"ship it"}`))
	b.IngestLog("task status=start request_id=req-merged repo=git@github.com:acme/repo.git")
	b.IngestLog("task status=completed request_id=req-merged workspace=/tmp/run branch=agent00-ship pr_url=https://github.com/acme/repo/pull/42")

	if err := b.MarkTaskPRMerged("req-merged", "2026-05-07T12:04:00Z"); err != nil {
		t.Fatalf("MarkTaskPRMerged() error = %v", err)
	}

	snap := b.Snapshot()
	if got, want := snap.Stats.ActiveTasks, 0; got != want {
		t.Fatalf("Stats.ActiveTasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.CompletedTasks, 1; got != want {
		t.Fatalf("Stats.CompletedTasks = %d, want %d", got, want)
	}
	if got, want := snap.Repositories[0].Stats.ActiveTasks, 0; got != want {
		t.Fatalf("repository ActiveTasks = %d, want %d", got, want)
	}
	if got, want := snap.Repositories[0].Stats.CompletedTasks, 1; got != want {
		t.Fatalf("repository CompletedTasks = %d, want %d", got, want)
	}
}

func TestBrokerTracksStoppedAndPausedTasksSeparatelyFromFailed(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-stopped", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"stop me"}`))
	b.IngestLog("task status=start request_id=req-stopped repo=git@github.com:acme/repo.git")
	b.IngestLog(`task status=stopped request_id=req-stopped err="task stopped by operator"`)

	b.RecordTaskRunConfig("req-paused", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"pause me"}`))
	b.IngestLog("task status=start request_id=req-paused repo=git@github.com:acme/repo.git")
	b.IngestLog("task status=paused request_id=req-paused")

	b.RecordTaskRunConfig("req-running", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"keep running"}`))
	b.IngestLog("task status=start request_id=req-running repo=git@github.com:acme/repo.git")

	b.RecordTaskRunConfig("req-pending", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"queue me"}`))

	snap := b.Snapshot()
	if got, want := snap.Stats.StoppedTasks, 1; got != want {
		t.Fatalf("Stats.StoppedTasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.FailedTasks, 0; got != want {
		t.Fatalf("Stats.FailedTasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.PausedTasks, 1; got != want {
		t.Fatalf("Stats.PausedTasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.RunningTasks, 1; got != want {
		t.Fatalf("Stats.RunningTasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.PendingTasks, 1; got != want {
		t.Fatalf("Stats.PendingTasks = %d, want %d", got, want)
	}
	if got, want := snap.Stats.ActiveTasks, 3; got != want {
		t.Fatalf("Stats.ActiveTasks = %d, want %d", got, want)
	}
}

func TestBrokerNormalizesLegacyOKTerminalStatusToCompleted(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-legacy")
	b.IngestLog("task status=ok request_id=req-legacy workspace=/tmp/run branch=agent00-legacy")

	snap := b.Snapshot()
	if got, want := len(snap.Tasks), 1; got != want {
		t.Fatalf("len(tasks) = %d, want %d", got, want)
	}
	if got, want := snap.Tasks[0].Status, "completed"; got != want {
		t.Fatalf("task.Status = %q, want %q", got, want)
	}
}

func TestBrokerDropsEmptyCommandPayloadMarkers(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-empty-b64"

	b.IngestLog("task status=start request_id=" + requestID)
	b.IngestLog("task request_id=req-empty-b64 cmd phase=codex name=codex stream=stderr b64=")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got := len(snap.Tasks[0].Logs); got != 1 {
		t.Fatalf("len(task logs) = %d, want 1 start meta line only", got)
	}
	if got, want := len(snap.Events), 1; got != want {
		t.Fatalf("len(events) = %d, want %d (drop empty command marker event)", got, want)
	}
}

func TestBrokerDropsInvalidCommandPayloadMarkers(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-invalid-b64"

	b.IngestLog("task status=start request_id=" + requestID)
	b.IngestLog("task request_id=req-invalid-b64 cmd phase=codex name=codex stream=stderr b64=%%%")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got := len(snap.Tasks[0].Logs); got != 1 {
		t.Fatalf("len(task logs) = %d, want 1 start meta line only", got)
	}
	if got, want := len(snap.Events), 1; got != want {
		t.Fatalf("len(events) = %d, want %d (drop invalid command marker event)", got, want)
	}
}

func TestBrokerTracksSchedulerResourceWindow(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("debug scheduler status=window state=steady cpu=12.5 memory=48.2 disk_io_mb_s=3.7 allowed=2 max=4 running=1 queue_depth=0")

	snap := b.Snapshot()
	if got := snap.Resources.CPUPercent; got != 12.5 {
		t.Fatalf("resources.cpu_percent = %v, want 12.5", got)
	}
	if got := snap.Resources.MemoryPercent; got != 48.2 {
		t.Fatalf("resources.memory_percent = %v, want 48.2", got)
	}
	if got := snap.Resources.DiskIOMBs; got != 3.7 {
		t.Fatalf("resources.disk_io_mb_s = %v, want 3.7", got)
	}
	if snap.Resources.UpdatedAt == "" {
		t.Fatal("resources.updated_at = empty, want timestamp")
	}
}

func TestBrokerCapturesPRURLFromStageLine(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-pr skill=code_for_me repo=git@github.com:acme/repo.git")
	b.IngestLog("task request_id=req-pr stage=pr status=ok pr_url=https://github.com/acme/repo/pull/101")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].PRURL != "https://github.com/acme/repo/pull/101" {
		t.Fatalf("task.PRURL = %q", snap.Tasks[0].PRURL)
	}
}

func TestBrokerCapturesReviewPRURLFromRunConfig(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-review", []byte(`{"repoUrl":"https://github.com/acme/repo.git","baseBranch":"main","review":{"prUrl":"https://github.com/acme/repo/pull/42"}}`))

	task, ok := b.Task("req-review")
	if !ok {
		t.Fatal("Task(req-review) ok = false, want true")
	}
	if got, want := task.PRURL, "https://github.com/acme/repo/pull/42"; got != want {
		t.Fatalf("task.PRURL = %q, want %q", got, want)
	}
	if got, want := task.Workflow, "code-review"; got != want {
		t.Fatalf("task.Workflow = %q, want %q", got, want)
	}
}

func TestBrokerQueuesWorkspaceAllocationRetry(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	const start = "task status=start request_id=local-retry explicit_terminal=true"
	b.IngestLog(start)
	b.IngestLog(`task request_id=local-retry stage=workspace status=error err="no space left on device"`)
	b.IngestLog(`task status=warn action=retry_workspace_allocation request_id=local-retry err="no space left on device"`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.Status != "pending" || task.StageStatus != "queued" || task.Error != "" {
		t.Fatalf("retry task status = %q, stage status = %q, error = %q", task.Status, task.StageStatus, task.Error)
	}
	if snap.Stats.PendingTasks != 1 || snap.Stats.RunningTasks != 0 {
		t.Fatalf("retry counts: pending = %d, running = %d", snap.Stats.PendingTasks, snap.Stats.RunningTasks)
	}

	b.IngestLog(start)
	snap = b.Snapshot()
	if snap.Tasks[0].Status != "running" || snap.Stats.PendingTasks != 0 || snap.Stats.RunningTasks != 1 {
		t.Fatalf("restarted task status = %q, pending = %d, running = %d", snap.Tasks[0].Status, snap.Stats.PendingTasks, snap.Stats.RunningTasks)
	}
}

func TestBrokerCapturesAutoMergeFromQueuedReviewConfig(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-auto-merge", []byte(`{"repo":"acme/repo","review":{"prNumber":42,"autoMerge":true}}`))
	b.RecordTaskRunConfig("req-manual", []byte(`{"repo":"acme/repo","review":{"prNumber":43}}`))

	autoMergeTask, ok := b.Task("req-auto-merge")
	if !ok {
		t.Fatal("Task(req-auto-merge) ok = false, want true")
	}
	if !autoMergeTask.AutoMerge {
		t.Fatal("Task(req-auto-merge).AutoMerge = false, want true")
	}
	manualTask, ok := b.Task("req-manual")
	if !ok {
		t.Fatal("Task(req-manual) ok = false, want true")
	}
	if manualTask.AutoMerge {
		t.Fatal("Task(req-manual).AutoMerge = true, want false")
	}
}

func TestBrokerCapturesFirstPRURLFromCompletedPRURLsField(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-prs skill=code_for_me repo=git@github.com:acme/repo.git")
	b.IngestLog("task status=completed request_id=req-prs workspace=/tmp/run branch=agent00-feature pr_url= pr_urls=https://github.com/acme/repo-a/pull/101,https://github.com/acme/repo-b/pull/202")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got, want := snap.Tasks[0].PRURL, "https://github.com/acme/repo-a/pull/101"; got != want {
		t.Fatalf("task.PRURL = %q, want %q", got, want)
	}
}

func TestBrokerFallsBackToSingleRepoWhenReposFieldMissing(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-single skill=code_for_me repo=git@github.com:acme/repo.git")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got, want := len(snap.Tasks[0].Repos), 1; got != want {
		t.Fatalf("len(task.Repos) = %d, want %d", got, want)
	}
	if snap.Tasks[0].Repos[0] != "git@github.com:acme/repo.git" {
		t.Fatalf("task.Repos = %#v", snap.Tasks[0].Repos)
	}
}

func TestBrokerParsesQuotedErrorText(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog(`task status=error request_id=req-err exit_code=50 err="codex exploded at step two"`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].Status != "error" {
		t.Fatalf("status = %q, want error", snap.Tasks[0].Status)
	}
	if snap.Tasks[0].ExitCode != 50 {
		t.Fatalf("exit code = %d, want 50", snap.Tasks[0].ExitCode)
	}
	if snap.Tasks[0].Error != "codex exploded at step two" {
		t.Fatalf("error = %q", snap.Tasks[0].Error)
	}
}

func TestBrokerParsesQuotedErrorTextWithEscapedQuotes(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog(`task status=error request_id=req-err-escaped exit_code=10 err="decode run config payload: json: unknown field \"branch\""`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].Error != `decode run config payload: json: unknown field "branch"` {
		t.Fatalf("error = %q", snap.Tasks[0].Error)
	}
}

func TestBrokerParsesStageErrorText(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-stage-err")
	b.IngestLog(`task request_id=req-stage-err stage=codex status=error err="codex command failed"`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.Status != "error" {
		t.Fatalf("status = %q, want error", task.Status)
	}
	if task.Stage != "codex" {
		t.Fatalf("stage = %q, want codex", task.Stage)
	}
	if task.StageStatus != "error" {
		t.Fatalf("stage status = %q, want error", task.StageStatus)
	}
	if task.Error != "codex command failed" {
		t.Fatalf("error = %q", task.Error)
	}
}

func TestBrokerKeepsAgentInvocationStageErrorRunning(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-agent-stage-err")
	b.IngestLog(`task request_id=req-agent-stage-err stage=codex status=error agent_run_id=agent-implementation-1 agent_harness=codex mode=implementation attempt=1 repo=repo repo_dir=repo err="sandbox blocked local commands"`)
	b.IngestLog(`task request_id=req-agent-stage-err stage=codex status=warn action=retry_without_sandbox agent_run_id=agent-implementation-1 agent_harness=codex mode=implementation attempt=1 repo=repo repo_dir=repo`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.Status != "running" {
		t.Fatalf("status = %q, want running", task.Status)
	}
	if task.Stage != "codex" {
		t.Fatalf("stage = %q, want codex", task.Stage)
	}
	if task.StageStatus != "warn" {
		t.Fatalf("stage status = %q, want warn after retry log", task.StageStatus)
	}
	if task.Error != "" {
		t.Fatalf("error = %q, want empty parent task error", task.Error)
	}
}

func TestBrokerParsesStageErrorTextWithEscapedQuotes(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-stage-err-quoted")
	b.IngestLog(`task request_id=req-stage-err-quoted stage=codex status=error err="decode run config payload: json: unknown field \"branch\""`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].Error != `decode run config payload: json: unknown field "branch"` {
		t.Fatalf("error = %q", snap.Tasks[0].Error)
	}
}

func TestBrokerIgnoresNestedRequestMetadataInsideQuotedCommandText(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "local-1712345678-000001"
	b.IngestLog("task status=start request_id=" + requestID)
	b.IngestLog(`task request_id=local-1712345678-000001 cmd phase=codex name=codex stream=stderr text="task status=error request_id=req-err-ws err=\"clone failed\""`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.RequestID != requestID {
		t.Fatalf("task.RequestID = %q, want %q", task.RequestID, requestID)
	}
	if task.Status != "running" {
		t.Fatalf("task.Status = %q, want running", task.Status)
	}
	if task.Error != "" {
		t.Fatalf("task.Error = %q, want empty", task.Error)
	}
}

func TestBrokerClearsRecoveredCheckError(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	b.IngestLog("task status=start request_id=recovered")
	b.IngestLog(`task request_id=recovered stage=checks status=warn err="no required checks reported"`)
	b.IngestLog("task request_id=recovered stage=checks status=ok reason=latest_snapshot")
	if got := b.Snapshot().Tasks[0]; got.Error != "" || got.Status != "running" {
		t.Fatalf("successful fallback retains error: %+v", got)
	}
	b.IngestLog(`task request_id=recovered event=command_finish phase=git name=git outcome=error elapsed_ms=4`)
	if got := b.Snapshot().Tasks[0]; got.Status != "running" || got.Stage != "checks" {
		t.Fatal("command timing changed workflow status")
	}
	b.IngestLog(`task status=error request_id=recovered err="push failed"`)
	if got := b.Snapshot().Tasks[0]; got.Error != "push failed" || got.Status != "error" {
		t.Fatal("terminal failure was hidden")
	}
}

func TestBrokerCapturesWorkspaceAndBranchFromErrorStatus(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog(`task status=error request_id=req-err-ws exit_code=40 workspace=/tmp/run-x branch=agent00-fix pr_url=https://github.com/acme/repo/pull/55 err="clone failed"`)

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.WorkspaceDir != "/tmp/run-x" {
		t.Fatalf("workspace = %q, want /tmp/run-x", task.WorkspaceDir)
	}
	if task.Branch != "agent00-fix" {
		t.Fatalf("branch = %q, want agent00-fix", task.Branch)
	}
	if task.PRURL != "https://github.com/acme/repo/pull/55" {
		t.Fatalf("pr_url = %q", task.PRURL)
	}
}

func TestBrokerDoesNotMaterializeDuplicateStatusAsTask(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=duplicate request_id=req-dup state=in_flight duplicate_of=req-100")

	snap := b.Snapshot()
	if len(snap.Tasks) != 0 {
		t.Fatalf("tasks = %d, want 0", len(snap.Tasks))
	}
	if len(snap.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(snap.Events))
	}
}

func TestBrokerDuplicateStatusDoesNotReplaceOriginalTask(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-100 skill=code_for_me repo=git@github.com:acme/repo.git")
	b.IngestLog(
		"task status=completed request_id=req-100 workspace=/tmp/run branch=agent00-fix pr_url=https://github.com/acme/repo/pull/55",
	)
	b.IngestLog("task status=duplicate request_id=req-dup state=completed duplicate_of=req-100")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.RequestID != "req-100" {
		t.Fatalf("request_id = %q, want req-100", task.RequestID)
	}
	if task.Status != "completed" {
		t.Fatalf("status = %q, want completed", task.Status)
	}
	if task.Error != "" {
		t.Fatalf("error = %q, want empty", task.Error)
	}
}

func TestBrokerSubscribeSignalsChanges(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	updates, cancel := b.Subscribe()
	defer cancel()

	b.IngestLog("task status=start request_id=req-1")

	select {
	case <-updates:
		// Expected.
	default:
		t.Fatal("expected update signal")
	}
}

func TestBrokerTaskRunConfigSupportsRerunMetadata(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-rerun"
	payload := []byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"main","targetSubdir":".","prompt":"rerun me"}`)

	b.RecordTaskRunConfig(requestID, payload)
	b.IngestLog("task status=start request_id=req-rerun skill=code_for_me repo=git@github.com:acme/repo.git")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if !snap.Tasks[0].CanRerun {
		t.Fatal("task.CanRerun = false, want true")
	}
	if snap.Tasks[0].Prompt != "rerun me" {
		t.Fatalf("task.Prompt = %q, want %q", snap.Tasks[0].Prompt, "rerun me")
	}
	if snap.Tasks[0].BaseBranch != "main" {
		t.Fatalf("task.BaseBranch = %q, want %q", snap.Tasks[0].BaseBranch, "main")
	}
	if snap.Tasks[0].Branch != "main" {
		t.Fatalf("task.Branch = %q, want %q", snap.Tasks[0].Branch, "main")
	}

	got, ok := b.TaskRunConfig(requestID)
	if !ok {
		t.Fatal("TaskRunConfig() found = false, want true")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("TaskRunConfig() = %q, want %q", string(got), string(payload))
	}

	got[0] = 'x'
	gotAgain, ok := b.TaskRunConfig(requestID)
	if !ok {
		t.Fatal("TaskRunConfig() second fetch found = false, want true")
	}
	if bytes.Equal(gotAgain, got) {
		t.Fatalf("TaskRunConfig() returned shared slice %q", string(gotAgain))
	}
}

func TestBrokerTaskRunConfigExposesReviewers(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-reviewer"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"fix tests","reviewers":["@OctoCat","none","octocat","hubot"],"githubHandle":"Hubot"}`))

	task, ok := b.Task(requestID)
	if !ok {
		t.Fatal("Task() found = false, want true")
	}
	want := []string{"OctoCat", "hubot"}
	if !slices.Equal(task.Reviewers, want) {
		t.Fatalf("Task.Reviewers = %#v, want %#v", task.Reviewers, want)
	}
}

func TestBrokerRecordTaskRunConfigCreatesPendingTaskAndNotifies(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	updates, cancel := b.Subscribe()
	defer cancel()

	requestID := "req-pending-config"
	payload := []byte(`{"repos":["git@github.com:acme/repo.git","git@github.com:acme/repo-two.git"],"baseBranch":"main","targetSubdir":".","prompt":"show immediately"}`)

	b.RecordTaskRunConfig(requestID, payload)

	select {
	case <-updates:
		// Expected.
	default:
		t.Fatal("expected update signal")
	}

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	task := snap.Tasks[0]
	if task.RequestID != requestID {
		t.Fatalf("task.RequestID = %q, want %q", task.RequestID, requestID)
	}
	if task.Status != "pending" {
		t.Fatalf("task.Status = %q, want pending", task.Status)
	}
	if task.Stage != "task" || task.StageStatus != "queued" {
		t.Fatalf("task stage = (%q, %q), want (task, queued)", task.Stage, task.StageStatus)
	}
	if task.Prompt != "show immediately" {
		t.Fatalf("task.Prompt = %q, want %q", task.Prompt, "show immediately")
	}
	if task.Repo != "git@github.com:acme/repo.git" {
		t.Fatalf("task.Repo = %q", task.Repo)
	}
	if got, want := strings.Join(task.Repos, ","), "git@github.com:acme/repo.git,git@github.com:acme/repo-two.git"; got != want {
		t.Fatalf("task.Repos = %q, want %q", got, want)
	}
	if got, want := strings.Join(snap.PromptedRepos, ","), "git@github.com:acme/repo.git,git@github.com:acme/repo-two.git"; got != want {
		t.Fatalf("snapshot.PromptedRepos = %q, want %q", got, want)
	}
	if task.BaseBranch != "main" || task.Branch != "main" {
		t.Fatalf("task branches = (%q, %q), want main", task.BaseBranch, task.Branch)
	}
	if !task.CanRerun {
		t.Fatal("task.CanRerun = false, want true")
	}
}

func TestBrokerPreservesWorkflowTriggerForIssueTask(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	const issueURL = "https://github.com/example/project/issues/42"
	b.RecordTaskRunConfigWithSource("local-workflow-issue", []byte(`{
		"workflow":{"id":"issue-workflow","eventId":"event-42","policyHash":"runtime-policy"},
		"githubIssue":{"number":42,"url":"`+issueURL+`"}
	}`), "github_issues")
	b.DropTaskRunConfig("local-workflow-issue")
	b.RecordTaskRunConfig("local-library", []byte(`{"libraryTaskName":"unit-test-coverage"}`))

	task, ok := b.Task("local-workflow-issue")
	if !ok || !task.WorkflowTriggered || task.Source != "github_issues" || task.GitHubIssueURL != issueURL {
		t.Fatalf("workflow issue task lost its origin or issue metadata: %+v", task)
	}
	for _, task := range b.Snapshot().Tasks {
		if want := task.RequestID == "local-workflow-issue"; task.WorkflowTriggered != want {
			t.Errorf("task %s workflow trigger = %t, want %t", task.RequestID, task.WorkflowTriggered, want)
		}
	}
}

func TestBrokerRecordTaskRunConfigWithSource(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "local-source-config"
	payload := []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"show source"}`)

	b.RecordTaskRunConfigWithSource(requestID, payload, "chat")

	task, ok := b.Task(requestID)
	if !ok {
		t.Fatal("Task() found = false, want true")
	}
	if got, want := task.Source, "chat"; got != want {
		t.Fatalf("task.Source = %q, want %q", got, want)
	}

	b.RecordTaskSource(requestID, "json")
	task, ok = b.Task(requestID)
	if !ok {
		t.Fatal("Task() after source update found = false, want true")
	}
	if got, want := task.Source, "json"; got != want {
		t.Fatalf("task.Source after update = %q, want %q", got, want)
	}
}

func TestBrokerTracksPromptedReposFromDispatchLogs(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-42 repos=git@github.com:acme/repo.git,https://github.com/acme/repo-two")

	snap := b.Snapshot()
	if got, want := strings.Join(snap.PromptedRepos, ","), "git@github.com:acme/repo.git,https://github.com/acme/repo-two"; got != want {
		t.Fatalf("snapshot.PromptedRepos = %q, want %q", got, want)
	}
}

func TestBrokerTaskRunConfigSupportsBranchAlias(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-rerun-branch-alias"
	payload := []byte(`{"repo":"git@github.com:acme/repo.git","branch":"release/2026.04","targetSubdir":".","prompt":"rerun me"}`)

	b.RecordTaskRunConfig(requestID, payload)
	b.IngestLog("task status=start request_id=req-rerun-branch-alias skill=code_for_me repo=git@github.com:acme/repo.git")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got, want := snap.Tasks[0].Branch, "release/2026.04"; got != want {
		t.Fatalf("task.Branch = %q, want %q", got, want)
	}
	if got, want := snap.Tasks[0].BaseBranch, "release/2026.04"; got != want {
		t.Fatalf("task.BaseBranch = %q, want %q", got, want)
	}
}

func TestBrokerUpdatesTaskBranchFromStageLogsWhileRetainingBaseBranch(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-branch-transition"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"main","prompt":"rerun me"}`))
	b.IngestLog("task status=start request_id=req-branch-transition skill=code_for_me repo=git@github.com:acme/repo.git")
	b.IngestLog("task request_id=req-branch-transition stage=git status=ok action=branch branch=agent00-branch-transition")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got, want := snap.Tasks[0].BaseBranch, "main"; got != want {
		t.Fatalf("task.BaseBranch = %q, want %q", got, want)
	}
	if got, want := snap.Tasks[0].Branch, "agent00-branch-transition"; got != want {
		t.Fatalf("task.Branch = %q, want %q", got, want)
	}
}

func TestBrokerCloseTaskRemovesCompletedTaskAndPreservesRunConfig(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-close"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"close me"}`))
	b.IngestLog("task status=start request_id=req-close")
	b.IngestLog("task status=completed request_id=req-close workspace=/tmp/run branch=agent00-close")

	if err := b.CloseTask(requestID); err != nil {
		t.Fatalf("CloseTask() error = %v", err)
	}

	if _, ok := b.TaskRunConfig(requestID); !ok {
		t.Fatal("TaskRunConfig() found = false after CloseTask(), want true")
	}

	snap := b.Snapshot()
	if len(snap.Tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(snap.Tasks))
	}
}

func TestBrokerCloseTaskIgnoresLateCleanupLogs(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-close-cleanup"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"close me"}`))
	b.IngestLog("task status=start request_id=req-close-cleanup")
	b.IngestLog("task status=completed request_id=req-close-cleanup workspace=/tmp/run branch=agent00-close-cleanup")

	if err := b.CloseTask(requestID); err != nil {
		t.Fatalf("CloseTask() error = %v", err)
	}

	b.IngestLog("task status=ok action=task_close_cleanup request_id=req-close-cleanup log_dir=/tmp/.log/req/close/cleanup")

	snap := b.Snapshot()
	if len(snap.Tasks) != 0 {
		t.Fatalf("len(tasks) after cleanup log = %d, want 0", len(snap.Tasks))
	}
}

func TestBrokerIgnoresActionOnlyDispatchStatusForTerminalState(t *testing.T) {
	t.Parallel()

	b := NewBroker()

	b.IngestLog("task status=start request_id=req-follow-up")
	b.IngestLog(`task status=error request_id=req-follow-up exit_code=50 err="codex failed"`)
	b.IngestLog("task status=ok action=queue_failure_followup request_id=req-follow-up follow_up_request_id=req-fix")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(snap.Tasks))
	}
	if got := snap.Tasks[0].Status; got != "error" {
		t.Fatalf("task.Status = %q, want error", got)
	}
}

func TestBrokerClosedTaskTombstoneExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }

	requestID := "req-reuse-after-close"
	b.IngestLog("task status=stopped request_id=req-reuse-after-close err=\"task stopped by operator\"")
	if err := b.CloseTask(requestID); err != nil {
		t.Fatalf("CloseTask() error = %v", err)
	}

	b.IngestLog("task status=start request_id=req-reuse-after-close")
	if got := len(b.Snapshot().Tasks); got != 0 {
		t.Fatalf("len(tasks) before tombstone expiry = %d, want 0", got)
	}

	now = now.Add(defaultClosedTaskRetention + time.Second)
	b.IngestLog("task status=start request_id=req-reuse-after-close")

	snap := b.Snapshot()
	if got := len(snap.Tasks); got != 1 {
		t.Fatalf("len(tasks) after tombstone expiry = %d, want 1", got)
	}
	if got := snap.Tasks[0].RequestID; got != requestID {
		t.Fatalf("task.RequestID = %q, want %q", got, requestID)
	}
}

func TestBrokerCloseTaskRejectsIncompleteTask(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-running"
	b.IngestLog("task status=start request_id=req-running")

	err := b.CloseTask(requestID)
	if !errors.Is(err, ErrTaskNotCompleted) {
		t.Fatalf("CloseTask() error = %v, want %v", err, ErrTaskNotCompleted)
	}

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(snap.Tasks))
	}
}

func TestBrokerCloseTaskMissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	err := b.CloseTask("req-missing")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CloseTask() error = %v, want %v", err, ErrTaskNotFound)
	}
}

func TestBrokerTracksPausedResumedAndStoppedStatuses(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=paused request_id=req-control")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("len(tasks) after pause = %d, want 1", len(snap.Tasks))
	}
	if got := snap.Tasks[0].Status; got != "paused" {
		t.Fatalf("status after pause = %q, want %q", got, "paused")
	}

	b.IngestLog("task status=resumed request_id=req-control")
	snap = b.Snapshot()
	if got := snap.Tasks[0].Status; got != "pending" {
		t.Fatalf("status after resume = %q, want %q", got, "pending")
	}

	b.IngestLog(`task status=stopped request_id=req-control err="task was stopped by operator"`)
	snap = b.Snapshot()
	if got := snap.Tasks[0].Status; got != "stopped" {
		t.Fatalf("status after stop = %q, want %q", got, "stopped")
	}
	if got := snap.Tasks[0].Error; got != "task was stopped by operator" {
		t.Fatalf("error after stop = %q, want %q", got, "task was stopped by operator")
	}
}

func TestBrokerCloseTaskAllowsStoppedTasks(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-stopped-close"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"stop me"}`))
	b.IngestLog(`task status=stopped request_id=req-stopped-close err="task stopped by operator"`)

	if err := b.CloseTask(requestID); err != nil {
		t.Fatalf("CloseTask() for stopped task error = %v", err)
	}
	if _, ok := b.TaskRunConfig(requestID); !ok {
		t.Fatal("TaskRunConfig() found = false after CloseTask(stopped), want true")
	}
}

func TestBrokerTracksTaskAttemptsAcrossRerunsAndFailures(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-attempt")
	b.IngestLog(`task status=error request_id=req-attempt exit_code=1 err="first failure"`)
	b.RecordTaskRerunAttempt("req-attempt", "req-attempt-rerun")
	b.IngestLog("task status=start request_id=req-attempt-rerun")
	b.IngestLog("task status=completed request_id=req-attempt-rerun workspace=/tmp/run branch=agent00-attempt")

	attempts := b.TaskAttempts("req-attempt")
	if len(attempts) != 2 {
		t.Fatalf("len(attempts) = %d, want 2: %#v", len(attempts), attempts)
	}
	if got, want := attempts[0].RequestID, "req-attempt"; got != want {
		t.Fatalf("attempt[0].RequestID = %q, want %q", got, want)
	}
	if got, want := attempts[0].Status, "error"; got != want {
		t.Fatalf("attempt[0].Status = %q, want %q", got, want)
	}
	if got, want := attempts[0].Error, "first failure"; got != want {
		t.Fatalf("attempt[0].Error = %q, want %q", got, want)
	}
	if got, want := attempts[1].RequestID, "req-attempt-rerun"; got != want {
		t.Fatalf("attempt[1].RequestID = %q, want %q", got, want)
	}
	if got, want := attempts[1].RerunOf, "req-attempt"; got != want {
		t.Fatalf("attempt[1].RerunOf = %q, want %q", got, want)
	}
	if got, want := attempts[1].Status, "completed"; got != want {
		t.Fatalf("attempt[1].Status = %q, want %q", got, want)
	}

	rerunAttempts := b.TaskAttempts("req-attempt-rerun")
	if len(rerunAttempts) != len(attempts) {
		t.Fatalf("len(rerun attempts) = %d, want %d", len(rerunAttempts), len(attempts))
	}

	snap := b.Snapshot()
	if got, want := snap.Stats.TotalTasks, 1; got != want {
		t.Fatalf("stats.total_tasks = %d, want %d (restart should collapse to one task)", got, want)
	}
	if got, want := snap.Stats.CompletedTasks, 1; got != want {
		t.Fatalf("stats.completed_tasks = %d, want %d (final state after restart)", got, want)
	}
	if got, want := snap.Stats.FailedTasks, 0; got != want {
		t.Fatalf("stats.failed_tasks = %d, want %d (original failure should not also be counted)", got, want)
	}
}

func TestBrokerForgetTaskRemovesAttemptsFromDashboardStats(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-forget", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"forget me"}`))
	b.IngestLog("task status=start request_id=req-forget")
	b.IngestLog("task status=completed request_id=req-forget workspace=/tmp/run branch=agent00-forget")

	if got, want := b.Snapshot().Stats.TotalTasks, 1; got != want {
		t.Fatalf("stats.total_tasks before forget = %d, want %d", got, want)
	}

	if err := b.ForgetTask("req-forget"); err != nil {
		t.Fatalf("ForgetTask() error = %v", err)
	}

	snap := b.Snapshot()
	if len(snap.Tasks) != 0 {
		t.Fatalf("len(tasks) after ForgetTask = %d, want 0", len(snap.Tasks))
	}
	if got, want := snap.Stats.TotalTasks, 0; got != want {
		t.Fatalf("stats.total_tasks after ForgetTask = %d, want %d (deleted task must drop out of metrics)", got, want)
	}
	if len(b.TaskAttempts("req-forget")) != 0 {
		t.Fatalf("TaskAttempts(req-forget) after ForgetTask = %#v, want empty", b.TaskAttempts("req-forget"))
	}
	if _, ok := b.TaskRunConfig("req-forget"); !ok {
		t.Fatal("TaskRunConfig() found = false after ForgetTask(), want true (run config retained)")
	}
}

func TestBrokerForgetTaskPreservesLiveRerunSibling(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-chain")
	b.IngestLog(`task status=error request_id=req-chain exit_code=1 err="first failure"`)
	b.RecordTaskRerunAttempt("req-chain", "req-chain-rerun")
	b.IngestLog("task status=start request_id=req-chain-rerun")

	if err := b.ForgetTask("req-chain"); err != nil {
		t.Fatalf("ForgetTask(original) error = %v", err)
	}

	if attempts := b.TaskAttempts("req-chain-rerun"); len(attempts) == 0 {
		t.Fatalf("TaskAttempts(req-chain-rerun) after forgetting original = empty, want still-live rerun retained")
	}

	snap := b.Snapshot()
	if got, want := snap.Stats.ActiveTasks, 1; got != want {
		t.Fatalf("stats.active_tasks after forgetting original = %d, want %d (rerun still running)", got, want)
	}

	b.IngestLog("task status=completed request_id=req-chain-rerun workspace=/tmp/run branch=agent00-chain")

	snap = b.Snapshot()
	if got, want := snap.Stats.TotalTasks, 1; got != want {
		t.Fatalf("stats.total_tasks after rerun completes = %d, want %d", got, want)
	}
	if got, want := snap.Stats.CompletedTasks, 1; got != want {
		t.Fatalf("stats.completed_tasks after rerun completes = %d, want %d", got, want)
	}
	if got, want := snap.Stats.FailedTasks, 0; got != want {
		t.Fatalf("stats.failed_tasks after rerun completes = %d, want %d (forgotten failure must not resurface)", got, want)
	}
}

func TestBrokerForgetTaskRejectsIncompleteTask(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-forget-running")

	if err := b.ForgetTask("req-forget-running"); !errors.Is(err, ErrTaskNotCompleted) {
		t.Fatalf("ForgetTask() error = %v, want %v", err, ErrTaskNotCompleted)
	}
	if got, want := b.Snapshot().Stats.TotalTasks, 1; got != want {
		t.Fatalf("stats.total_tasks = %d, want %d (rejected forget must not touch metrics)", got, want)
	}
}

func TestBrokerRemovesCompletedTasksAfterTimeout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }

	b.RecordTaskRunConfig("req-ok", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"keep me visible"}`))
	b.IngestLog("task status=start request_id=req-ok")
	b.IngestLog("task status=completed request_id=req-ok workspace=/tmp/run branch=agent00-cleanup")

	if got := len(b.Snapshot().Tasks); got != 1 {
		t.Fatalf("len(tasks) before timeout = %d, want 1", got)
	}

	now = now.Add(defaultTerminalTaskTimeout - time.Second)
	if got := len(b.Snapshot().Tasks); got != 1 {
		t.Fatalf("len(tasks) before timeout expiry = %d, want 1", got)
	}

	now = now.Add(time.Second)
	snap := b.Snapshot()
	if got := len(snap.Tasks); got != 0 {
		t.Fatalf("len(tasks) after timeout = %d, want 0", got)
	}
	if _, ok := b.TaskRunConfig("req-ok"); !ok {
		t.Fatal("TaskRunConfig() found = false for expired completed task, want true")
	}

	b.IngestLog("task status=ok action=task_close_cleanup request_id=req-ok log_dir=/tmp/.log/req/ok")
	if got := len(b.Snapshot().Tasks); got != 0 {
		t.Fatalf("len(tasks) after late cleanup log = %d, want 0", got)
	}
}

func TestBrokerRemovesFailedTasksAfterTimeout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }

	b.RecordTaskRunConfig("req-error", []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"fail visibly"}`))
	b.IngestLog("task status=start request_id=req-error")
	b.IngestLog(`task status=error request_id=req-error exit_code=50 err="codex exploded"`)

	now = now.Add(defaultTerminalTaskTimeout)
	snap := b.Snapshot()
	if got := len(snap.Tasks); got != 0 {
		t.Fatalf("len(tasks) after timeout for failed task = %d, want 0", got)
	}
	if _, ok := b.TaskRunConfig("req-error"); !ok {
		t.Fatal("TaskRunConfig() found = false for expired failed task, want true")
	}
}

func TestBrokerPreservesReviewCycleWarningOnCompletedTask(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-review-warning")
	b.IngestLog("task request_id=req-review-warning stage=review status=warn action=leave_draft reason=fix_passes_exhausted fix_passes=10")
	b.IngestLog("task status=completed request_id=req-review-warning pr_url=https://example.com/pull/1")

	task, ok := b.Task("req-review-warning")
	if !ok {
		t.Fatal("Task() found = false, want true")
	}
	if task.Status != "completed" {
		t.Fatalf("task.Status = %q, want completed", task.Status)
	}
	if want := "Post-task review findings remain. Pull request left in draft for human review."; task.Warning != want {
		t.Fatalf("task.Warning = %q, want %q", task.Warning, want)
	}
}

func TestBrokerPreservesMergedDuringReviewWarningOnCompletedTask(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=start request_id=req-review-merged")
	b.IngestLog("task request_id=req-review-merged stage=review status=warn action=stop reason=pr_merged pass=2 total=5")
	b.IngestLog("task status=completed request_id=req-review-merged pr_url=https://example.com/pull/1")

	task, ok := b.Task("req-review-merged")
	if !ok {
		t.Fatal("Task() found = false, want true")
	}
	if task.Status != "completed" {
		t.Fatalf("task.Status = %q, want completed", task.Status)
	}
	if want := "Pull request merged externally during review. Review interrupted."; task.Warning != want {
		t.Fatalf("task.Warning = %q, want %q", task.Warning, want)
	}
}

func TestBrokerAppliesPromptWhenConfigRecordedAfterTaskStart(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-after-start"

	b.IngestLog("task status=start request_id=req-after-start skill=code_for_me repo=git@github.com:acme/repo.git")
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"release/2026.04","prompt":"late prompt value"}`))

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].Prompt != "late prompt value" {
		t.Fatalf("task.Prompt = %q, want %q", snap.Tasks[0].Prompt, "late prompt value")
	}
	if snap.Tasks[0].BaseBranch != "release/2026.04" {
		t.Fatalf("task.BaseBranch = %q, want %q", snap.Tasks[0].BaseBranch, "release/2026.04")
	}
	if snap.Tasks[0].Branch != "release/2026.04" {
		t.Fatalf("task.Branch = %q, want %q", snap.Tasks[0].Branch, "release/2026.04")
	}
	if !snap.Tasks[0].PromptIsUserInput {
		t.Fatal("task.PromptIsUserInput = false, want true for user-entered prompt")
	}
}

func TestBrokerMarksFailureFollowUpPromptAsSystemGenerated(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-after-start-failure-review"

	b.IngestLog("task status=start request_id=req-after-start-failure-review skill=code_for_me repo=git@github.com:acme/repo.git")
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"main","prompt":"Review the failing log paths first, identify every root cause behind the failed task, fix the underlying agent_00 application issues in this repository, validate locally where possible, and summarize the verified results. Treat the original task prompt as failure context only; do not implement that requested product change here unless it is required to fix agent_00 failure handling.\n\nObserved failure context:\n- error=boom"}`))

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].PromptIsUserInput {
		t.Fatal("task.PromptIsUserInput = true, want false for failure follow-up prompt")
	}
}

func TestBrokerResolvesLibraryTaskDisplayTextFromRunConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HARNESS_LIBRARY_DIR", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "coverage.json"), []byte(`{
  "name": "unit-test-coverage",
  "displayName": "100% Unit Test Coverage",
  "description": "Raise coverage.",
  "prompt": "Raise coverage."
}`), 0o644); err != nil {
		t.Fatalf("write library task: %v", err)
	}

	b := NewBroker()
	requestID := "req-library-task"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"main","libraryTaskName":"unit-test-coverage"}`))
	b.IngestLog("task status=start request_id=req-library-task skill=library_task repo=git@github.com:acme/repo.git")

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if got, want := snap.Tasks[0].Prompt, "100% Unit Test Coverage\n\nRaise coverage."; got != want {
		t.Fatalf("task.Prompt = %q, want %q", got, want)
	}
	if !snap.Tasks[0].PromptIsUserInput {
		t.Fatal("task.PromptIsUserInput = false, want true for library prompt")
	}
}

func TestBrokerRecordsRejectedPromptSubmission(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := b.RecordRejectedPromptSubmission(
		[]byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"main","prompt":"fix broken prompt"}`),
		"invalid",
		errors.New("invalid run config: prompt failed checks"),
	)

	if requestID == "" {
		t.Fatal("RecordRejectedPromptSubmission() requestID = empty, want value")
	}

	snap := b.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(snap.Tasks))
	}

	task := snap.Tasks[0]
	if task.RequestID != requestID {
		t.Fatalf("task.RequestID = %q, want %q", task.RequestID, requestID)
	}
	if task.Status != "invalid" {
		t.Fatalf("task.Status = %q, want invalid", task.Status)
	}
	if task.Source != "prompt" {
		t.Fatalf("task.Source = %q, want prompt", task.Source)
	}
	if task.Prompt != "fix broken prompt" {
		t.Fatalf("task.Prompt = %q, want %q", task.Prompt, "fix broken prompt")
	}
	if task.Repo != "git@github.com:acme/repo.git" {
		t.Fatalf("task.Repo = %q, want %q", task.Repo, "git@github.com:acme/repo.git")
	}
	if task.BaseBranch != "main" {
		t.Fatalf("task.BaseBranch = %q, want main", task.BaseBranch)
	}
	if task.Branch != "main" {
		t.Fatalf("task.Branch = %q, want main", task.Branch)
	}
	if task.CanRerun {
		t.Fatal("task.CanRerun = true, want false")
	}
	if task.Error != "invalid run config: prompt failed checks" {
		t.Fatalf("task.Error = %q, want detailed failure", task.Error)
	}
	if len(task.Logs) != 1 || !strings.Contains(task.Logs[0].Text, "prompt submission failed: invalid run config: prompt failed checks") {
		t.Fatalf("task.Logs = %#v, want failure log entry", task.Logs)
	}
}

func TestBrokerRecordsRejectedPromptSubmissionWithSource(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := b.RecordRejectedPromptSubmissionWithSource(
		[]byte(`{"repo":"git@github.com:acme/repo.git","baseBranch":"main","prompt":"fix broken prompt"}`),
		"invalid",
		errors.New("invalid run config: prompt failed checks"),
		"json",
	)

	task, ok := b.Task(requestID)
	if !ok {
		t.Fatal("Task() found = false, want true")
	}
	if got, want := task.Source, "json"; got != want {
		t.Fatalf("task.Source = %q, want %q", got, want)
	}
}

func TestPromptFromRunConfigJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "valid prompt",
			raw:  []byte(`{"prompt":"run integration tests"}`),
			want: "run integration tests",
		},
		{
			name: "missing prompt",
			raw:  []byte(`{"repo":"git@github.com:acme/repo.git"}`),
			want: "",
		},
		{
			name: "invalid json",
			raw:  []byte(`{"prompt":"oops"`),
			want: "",
		},
		{
			name: "trimmed prompt",
			raw:  []byte(`{"prompt":"  refactor api  "}`),
			want: "refactor api",
		},
		{
			name: "library task display text",
			raw:  []byte(`{"libraryTaskName":"unit-test-coverage"}`),
			want: "100% Unit Test Coverage\n\nRaise coverage.",
		},
		{
			name: "canonical library task display text",
			raw:  []byte(`{"librarytaskname":"unit-test-coverage"}`),
			want: "100% Unit Test Coverage\n\nRaise coverage.",
		},
		{
			name: "library task display text wins over expanded prompt",
			raw:  []byte(`{"libraryTaskName":"unit-test-coverage","prompt":"Full execution prompt."}`),
			want: "100% Unit Test Coverage\n\nRaise coverage.",
		},
		{
			name: "library task falls back to prompt when metadata missing",
			raw:  []byte(`{"libraryTaskName":"missing-task","prompt":"Full execution prompt."}`),
			want: "Full execution prompt.",
		},
	}

	tmp := t.TempDir()
	t.Setenv("HARNESS_LIBRARY_DIR", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "coverage.json"), []byte(`{
  "name": "unit-test-coverage",
  "displayName": "100% Unit Test Coverage",
  "description": "Raise coverage.",
  "prompt": "Raise coverage."
}`), 0o644); err != nil {
		t.Fatalf("write library task: %v", err)
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := promptFromRunConfigJSON(tt.raw); got != tt.want {
				t.Fatalf("promptFromRunConfigJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBranchFromRunConfigJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "base branch camelCase",
			raw:  []byte(`{"baseBranch":"main"}`),
			want: "main",
		},
		{
			name: "branch alias",
			raw:  []byte(`{"branch":"release/2026.04"}`),
			want: "release/2026.04",
		},
		{
			name: "prefer base branch",
			raw:  []byte(`{"baseBranch":"main","branch":"feature-x"}`),
			want: "main",
		},
		{
			name: "missing branch",
			raw:  []byte(`{"prompt":"run tests"}`),
			want: "",
		},
		{
			name: "invalid json",
			raw:  []byte(`{"baseBranch":"main"`),
			want: "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := branchFromRunConfigJSON(tt.raw); got != tt.want {
				t.Fatalf("branchFromRunConfigJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBrokerCapsEventsWithoutGrowingSlice(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.maxEvents = 3

	for i := 1; i <= 5; i++ {
		b.IngestLog(fmt.Sprintf("task status=start request_id=req-%d", i))
	}

	snap := b.Snapshot()
	if got, want := len(snap.Events), 3; got != want {
		t.Fatalf("len(events) = %d, want %d", got, want)
	}
	if snap.Events[0].RequestID != "req-3" || snap.Events[1].RequestID != "req-4" || snap.Events[2].RequestID != "req-5" {
		t.Fatalf("events request ids = %#v", []string{
			snap.Events[0].RequestID,
			snap.Events[1].RequestID,
			snap.Events[2].RequestID,
		})
	}
}

func TestBrokerCapsTaskLogsWithoutGrowingSlice(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.maxTaskLog = 2

	b.IngestLog("task status=start request_id=req-cap")
	b.IngestLog("task request_id=req-cap stage=clone status=start")
	b.IngestLog("task request_id=req-cap stage=pr status=ok")

	snap := b.Snapshot()
	if got, want := len(snap.Tasks), 1; got != want {
		t.Fatalf("len(tasks) = %d, want %d", got, want)
	}
	logs := snap.Tasks[0].Logs
	if got, want := len(logs), 2; got != want {
		t.Fatalf("len(task logs) = %d, want %d", got, want)
	}
	if logs[0].Text != "task request_id=req-cap stage=clone status=start" {
		t.Fatalf("logs[0].Text = %q", logs[0].Text)
	}
	if logs[1].Text != "task request_id=req-cap stage=pr status=ok" {
		t.Fatalf("logs[1].Text = %q", logs[1].Text)
	}
}

func TestTaskShowsPublishedReviewEvidenceAndPendingGate(t *testing.T) {
	t.Parallel()
	broker := NewBroker()
	const url = "https://github.com/example/project/pull/12#pullrequestreview-50"
	broker.IngestLog("task status=start request_id=review-task explicit_terminal=true")
	broker.IngestLog("task request_id=review-task stage=review status=ok action=receipt review_url=" + url)
	broker.IngestLog("task status=completed request_id=review-task")
	for _, tc := range []struct{ reason, text string }{
		{"review_writeback_pending", "Waiting to confirm GitHub review writeback."},
		{"review_writeback_paused", "Review writeback paused by workflow settings."},
		{"review_threads_pending", "Waiting for unresolved review threads."},
	} {
		broker.IngestLog("workflow event=transition request_id=review-task status=awaiting_review reason=" + tc.reason)
		task, ok := broker.Task("review-task")
		if !ok || task.ReviewURL != url || task.ReviewGate != tc.text {
			t.Fatalf("task did not preserve review evidence and pending gate: %+v", task)
		}
	}
}
