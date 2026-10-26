package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---- nil-receiver and empty-argument guards -------------------------------
//
// Broker methods are written to be safe to call on a nil *Broker and with
// blank/empty arguments (the web/server layer leans on this so handlers do
// not need extra nil checks). These guards were largely untested directly.

func TestBrokerNilReceiverGuardsAreSafeNoOps(t *testing.T) {
	t.Parallel()

	var nilBroker *Broker

	nilBroker.IngestLog("task status=start request_id=req-1")
	nilBroker.NotifyCustomLibraryUpdated()
	nilBroker.RecordTaskRunConfig("req-1", []byte(`{"prompt":"x"}`))
	nilBroker.RecordTaskRunConfigWithSource("req-1", []byte(`{"prompt":"x"}`), "chat")
	nilBroker.RecordTaskSource("req-1", "chat")
	nilBroker.RecordReleaseFromTask(Task{RequestID: "req-1", PRURL: "https://github.com/acme/repo/pull/1"}, "")
	nilBroker.DropTaskRunConfig("req-1")
	nilBroker.SetGitHubRepositories(nil)

	if got := nilBroker.Snapshot(); got.GeneratedAt != "" || got.Tasks != nil {
		t.Fatalf("nil Broker.Snapshot() = %#v, want zero value", got)
	}
	if _, ok := nilBroker.Task("req-1"); ok {
		t.Fatal("nil Broker.Task() found = true, want false")
	}
	if got := nilBroker.RecordRejectedPromptSubmission(nil, "", nil); got != "" {
		t.Fatalf("nil Broker.RecordRejectedPromptSubmission() = %q, want empty", got)
	}
	if got := nilBroker.RecordRejectedPromptSubmissionWithSource(nil, "", nil, ""); got != "" {
		t.Fatalf("nil Broker.RecordRejectedPromptSubmissionWithSource() = %q, want empty", got)
	}
	if err := nilBroker.MarkTaskPRMerged("req-1", ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("nil Broker.MarkTaskPRMerged() error = %v, want %v", err, ErrTaskNotFound)
	}
	if got := nilBroker.TaskAttempts("req-1"); got != nil {
		t.Fatalf("nil Broker.TaskAttempts() = %#v, want nil", got)
	}
	nilBroker.RecordTaskRerunAttempt("req-1", "req-2")
	if err := nilBroker.CloseTask("req-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("nil Broker.CloseTask() error = %v, want %v", err, ErrTaskNotFound)
	}
	if err := nilBroker.ForgetTask("req-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("nil Broker.ForgetTask() error = %v, want %v", err, ErrTaskNotFound)
	}
	if _, ok := nilBroker.TaskRunConfig("req-1"); ok {
		t.Fatal("nil Broker.TaskRunConfig() found = true, want false")
	}

	ch, cancel := nilBroker.Subscribe()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("nil Broker.Subscribe() channel is open, want already closed")
		}
	default:
		t.Fatal("nil Broker.Subscribe() channel did not return immediately")
	}
	cancel() // must not panic

	if got := nilBroker.taskBaseBranchLocked("req-1"); got != "" {
		t.Fatalf("nil Broker.taskBaseBranchLocked() = %q, want empty", got)
	}
	if got := nilBroker.taskInitialBranchLocked("req-1"); got != "" {
		t.Fatalf("nil Broker.taskInitialBranchLocked() = %q, want empty", got)
	}
	if got := nilBroker.taskPromptLocked("req-1"); got != "" {
		t.Fatalf("nil Broker.taskPromptLocked() = %q, want empty", got)
	}
}

func TestBrokerEmptyRequestIDGuardsAreNoOps(t *testing.T) {
	t.Parallel()

	b := NewBroker()

	if _, ok := b.Task("   "); ok {
		t.Fatal("Task(blank) found = true, want false")
	}
	b.RecordTaskRunConfigWithSource("", []byte(`{"prompt":"x"}`), "chat")
	b.RecordTaskRunConfigWithSource("req-x", nil, "chat")
	if _, ok := b.TaskRunConfig("req-x"); ok {
		t.Fatal("TaskRunConfig(req-x) found = true after empty-payload call, want false")
	}
	b.RecordTaskSource("", "chat")
	b.RecordTaskSource("req-missing", "chat") // task does not exist: no-op
	b.DropTaskRunConfig("   ")
	if _, ok := b.TaskRunConfig(""); ok {
		t.Fatal("TaskRunConfig(blank) found = true, want false")
	}
	if got := b.TaskAttempts("   "); got != nil {
		t.Fatalf("TaskAttempts(blank) = %#v, want nil", got)
	}
	b.RecordTaskRerunAttempt("", "req-y")
	b.RecordTaskRerunAttempt("req-y", "")
	if err := b.CloseTask("   "); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CloseTask(blank) error = %v, want %v", err, ErrTaskNotFound)
	}
	if err := b.ForgetTask("   "); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("ForgetTask(blank) error = %v, want %v", err, ErrTaskNotFound)
	}
	if err := b.MarkTaskPRMerged("   ", ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("MarkTaskPRMerged(blank) error = %v, want %v", err, ErrTaskNotFound)
	}
}

func TestMarkTaskPRMergedRejectsMissingAndIncompleteTasks(t *testing.T) {
	t.Parallel()

	b := NewBroker()

	if err := b.MarkTaskPRMerged("req-missing", ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("MarkTaskPRMerged(missing) error = %v, want %v", err, ErrTaskNotFound)
	}

	b.IngestLog("task status=start request_id=req-running repo=acme/repo")
	if err := b.MarkTaskPRMerged("req-running", ""); !errors.Is(err, ErrTaskNotCompleted) {
		t.Fatalf("MarkTaskPRMerged(running) error = %v, want %v", err, ErrTaskNotCompleted)
	}
}

func TestDropTaskRunConfigRemovesStoredConfig(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-drop", []byte(`{"prompt":"drop me"}`))
	if _, ok := b.TaskRunConfig("req-drop"); !ok {
		t.Fatal("TaskRunConfig() found = false before drop, want true")
	}
	b.DropTaskRunConfig("req-drop")
	if _, ok := b.TaskRunConfig("req-drop"); ok {
		t.Fatal("TaskRunConfig() found = true after drop, want false")
	}
}

func TestForgetTaskHandlesOrphanedAttemptRoot(t *testing.T) {
	t.Parallel()

	// A task directly materialized without going through the normal
	// recordTaskAttemptLocked bookkeeping path has no attemptRoots entry;
	// forgetTaskAttemptsLocked must fall back to the request id itself to
	// find its (also directly materialized) attempt list.
	b := NewBroker()
	b.mu.Lock()
	b.tasks["req-orphan"] = &taskState{RequestID: "req-orphan", Status: "completed"}
	b.attempts["req-orphan"] = []taskAttemptState{{RequestID: "req-orphan", Status: "completed"}}
	b.mu.Unlock()

	if err := b.ForgetTask("req-orphan"); err != nil {
		t.Fatalf("ForgetTask(orphan) error = %v", err)
	}
	b.mu.RLock()
	_, stillPresent := b.tasks["req-orphan"]
	b.mu.RUnlock()
	if stillPresent {
		t.Fatal("ForgetTask(orphan) left task present, want removed")
	}
}

func TestCollapseAttemptsByRootLockedFallsBackToRequestIDWhenRootMissing(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	seen := map[string]taskAttemptState{
		"req-orphan": {RequestID: "req-orphan", Status: "pending", Seq: 1},
	}
	b.mu.Lock()
	collapsed := b.collapseAttemptsByRootLocked(seen)
	b.mu.Unlock()

	if _, ok := collapsed["req-orphan"]; !ok {
		t.Fatalf("collapseAttemptsByRootLocked() = %#v, want req-orphan collapsed under itself", collapsed)
	}
}

// ---- attemptIsNewer: the dedup/tie-break logic -----------------------------

func TestAttemptIsNewer(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		candidate taskAttemptState
		current   taskAttemptState
		want      bool
	}{
		{
			name:      "higher sequence wins even if its timestamp is older",
			candidate: taskAttemptState{Seq: 2, UpdatedAt: base},
			current:   taskAttemptState{Seq: 1, UpdatedAt: base.Add(time.Hour)},
			want:      true,
		},
		{
			name:      "lower sequence loses even if its timestamp is newer",
			candidate: taskAttemptState{Seq: 1, UpdatedAt: base.Add(time.Hour)},
			current:   taskAttemptState{Seq: 2, UpdatedAt: base},
			want:      false,
		},
		{
			name:      "same sequence: later update wins",
			candidate: taskAttemptState{Seq: 1, UpdatedAt: base.Add(time.Minute)},
			current:   taskAttemptState{Seq: 1, UpdatedAt: base},
			want:      true,
		},
		{
			name:      "same sequence: earlier update loses",
			candidate: taskAttemptState{Seq: 1, UpdatedAt: base},
			current:   taskAttemptState{Seq: 1, UpdatedAt: base.Add(time.Minute)},
			want:      false,
		},
		{
			name:      "same sequence and equal timestamps: tie-break by sequence (equal => false)",
			candidate: taskAttemptState{Seq: 1, UpdatedAt: base},
			current:   taskAttemptState{Seq: 1, UpdatedAt: base},
			want:      false,
		},
		{
			name:      "same sequence: candidate zero UpdatedAt falls back to StartedAt",
			candidate: taskAttemptState{Seq: 1, StartedAt: base.Add(time.Minute)},
			current:   taskAttemptState{Seq: 1, UpdatedAt: base},
			want:      true,
		},
		{
			name:      "same sequence: current zero UpdatedAt falls back to StartedAt",
			candidate: taskAttemptState{Seq: 1, UpdatedAt: base.Add(time.Minute)},
			current:   taskAttemptState{Seq: 1, StartedAt: base},
			want:      true,
		},
		{
			name:      "same sequence and both timestamps zero: tie-break by sequence (equal => false)",
			candidate: taskAttemptState{Seq: 1},
			current:   taskAttemptState{Seq: 1},
			want:      false,
		},
		{
			name:      "higher sequence wins even when candidate has no timestamps at all",
			candidate: taskAttemptState{Seq: 5},
			current:   taskAttemptState{Seq: 1, UpdatedAt: base.Add(24 * time.Hour)},
			want:      true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := attemptIsNewer(tt.candidate, tt.current); got != tt.want {
				t.Fatalf("attemptIsNewer(%#v, %#v) = %t, want %t", tt.candidate, tt.current, got, tt.want)
			}
		})
	}
}

func TestTaskAttemptRootLockedAssignsAndReusesRoot(t *testing.T) {
	t.Parallel()

	b := &Broker{}
	if got := b.taskAttemptRootLocked(""); got != "" {
		t.Fatalf("taskAttemptRootLocked(blank) = %q, want empty", got)
	}
	if got := b.taskAttemptRootLocked("req-1"); got != "req-1" {
		t.Fatalf("taskAttemptRootLocked(first call) = %q, want %q", got, "req-1")
	}
	// A nested rerun root assigned out-of-band must be reused rather than reset.
	b.attemptRoots["req-1"] = "req-root"
	if got := b.taskAttemptRootLocked("req-1"); got != "req-root" {
		t.Fatalf("taskAttemptRootLocked(existing root) = %q, want %q", got, "req-root")
	}
}

func TestRecordTaskAttemptLockedGuardsAndUpdatesInPlace(t *testing.T) {
	t.Parallel()

	b := &Broker{}
	now := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)

	// Both root and requestID blank must be a safe no-op even with nil maps.
	b.recordTaskAttemptLocked("", "", "", "pending", "", "", "", "", nil, "", now)

	b.recordTaskAttemptLocked("", "req-1", "", "pending", "", "wf1", "codex", "chat", []string{"acme/repo"}, "", now)
	attempts := b.attempts["req-1"]
	if len(attempts) != 1 || attempts[0].Workflow != "wf1" {
		t.Fatalf("attempts after create = %#v, want one entry with workflow wf1", attempts)
	}

	later := now.Add(time.Minute)
	b.recordTaskAttemptLocked("req-1", "req-1", "req-0", "completed", "boom", "wf2", "claude", "library", []string{"acme/repo2"}, "https://github.com/acme/repo/pull/9", later)
	attempts = b.attempts["req-1"]
	if len(attempts) != 1 {
		t.Fatalf("len(attempts) after update = %d, want 1 (update in place)", len(attempts))
	}
	got := attempts[0]
	if got.RerunOf != "req-0" || got.Workflow != "wf2" || got.Agent != "claude" || got.Source != "library" ||
		got.PRURL != "https://github.com/acme/repo/pull/9" || got.Status != "completed" || got.Error != "boom" ||
		!got.UpdatedAt.Equal(later) {
		t.Fatalf("attempt after update = %#v, want fully updated fields", got)
	}
}

// ---- updateTaskFromLineLocked: branches only reachable with direct access --
//
// The public IngestLog entrypoint filters noisy "cmd" lines via
// shouldDropNoisyCommandLine and duplicate-status lines via
// isDuplicateTaskLifecycleLine before updateTaskFromLineLocked ever runs.
// These direct calls exercise the defensive branches inside the function
// itself (exit-code parsing, duplicate detail messages, and the cmd/base64
// fallbacks), matching the existing repo pattern of unit testing *Locked
// helpers directly within the package.

func TestUpdateTaskFromLineLockedCapturesExitCodesAndDuplicateDetails(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	now := time.Now().UTC()

	stopped := &taskState{RequestID: "req-stopped-exit", Status: "running"}
	stoppedLine := `task status=stopped request_id=req-stopped-exit exit_code=7`
	b.updateTaskFromLineLocked(stopped, stoppedLine, parseKVFields(stoppedLine), now)
	if stopped.ExitCode != 7 {
		t.Fatalf("stopped.ExitCode = %d, want 7", stopped.ExitCode)
	}

	errored := &taskState{RequestID: "req-error-exit", Status: "running"}
	erroredLine := `task status=error request_id=req-error-exit exit_code=3`
	b.updateTaskFromLineLocked(errored, erroredLine, parseKVFields(erroredLine), now)
	if errored.ExitCode != 3 {
		t.Fatalf("errored.ExitCode = %d, want 3", errored.ExitCode)
	}

	invalid := &taskState{RequestID: "req-invalid", Status: "pending"}
	invalidLine := `task status=invalid request_id=req-invalid err="bad input"`
	b.updateTaskFromLineLocked(invalid, invalidLine, parseKVFields(invalidLine), now)
	if invalid.Status != "invalid" || invalid.Error != "bad input" {
		t.Fatalf("invalid task = %#v, want status=invalid error=bad input", invalid)
	}

	dup := &taskState{RequestID: "req-dup", Status: "pending"}
	dupLine := `task status=duplicate request_id=req-dup state=rejected duplicate_of=req-original`
	b.updateTaskFromLineLocked(dup, dupLine, parseKVFields(dupLine), now)
	wantErr := "duplicate submission ignored (state=rejected, duplicate_of=req-original)"
	if dup.Status != "duplicate" || dup.Error != wantErr {
		t.Fatalf("dup task = %#v, want status=duplicate error=%q", dup, wantErr)
	}

	dup2 := &taskState{RequestID: "req-dup2", Status: ""}
	dup2Line := `task status=duplicate request_id=req-dup2`
	b.updateTaskFromLineLocked(dup2, dup2Line, parseKVFields(dup2Line), now)
	if dup2.Status != "duplicate" || dup2.Error != "duplicate submission ignored" {
		t.Fatalf("dup2 task = %#v, want generic duplicate message", dup2)
	}
}

func TestUpdateTaskFromLineLockedCmdFallbacksSkipEmptyLogLines(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	now := time.Now().UTC()

	noText := &taskState{RequestID: "req-cmd-empty"}
	noTextLine := `task request_id=req-cmd-empty cmd exec`
	b.updateTaskFromLineLocked(noText, noTextLine, parseKVFields(noTextLine), now)
	if len(noText.Logs) != 0 {
		t.Fatalf("len(noText.Logs) = %d, want 0 (missing text and b64 must be a no-op)", len(noText.Logs))
	}

	badB64 := &taskState{RequestID: "req-cmd-badb64"}
	badB64Line := `task request_id=req-cmd-badb64 cmd exec b64=not-valid-base64!!`
	b.updateTaskFromLineLocked(badB64, badB64Line, parseKVFields(badB64Line), now)
	if len(badB64.Logs) != 0 {
		t.Fatalf("len(badB64.Logs) = %d, want 0 (invalid base64 must be a no-op)", len(badB64.Logs))
	}

	blankEncoded := base64.StdEncoding.EncodeToString([]byte("   "))
	blankDecoded := &taskState{RequestID: "req-cmd-blank"}
	blankLine := fmt.Sprintf(`task request_id=req-cmd-blank cmd exec b64=%s`, blankEncoded)
	b.updateTaskFromLineLocked(blankDecoded, blankLine, parseKVFields(blankLine), now)
	if len(blankDecoded.Logs) != 0 {
		t.Fatalf("len(blankDecoded.Logs) = %d, want 0 (blank decoded text must be a no-op)", len(blankDecoded.Logs))
	}
}

// ---- RecordTaskRunConfigWithSource: updating an already-materialized task -

func TestRecordTaskRunConfigWithSourceUpdatesExistingTaskFields(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-update"
	b.RecordTaskRunConfigWithSource(requestID, []byte(`{
		"repo":"acme/repo1",
		"baseBranch":"main",
		"prompt":"first",
		"libraryTaskName":"wf1",
		"agentHarness":"codex",
		"reviewers":["alice"]
	}`), "prompt")

	b.RecordTaskRunConfigWithSource(requestID, []byte(`{
		"repo":"acme/repo2",
		"baseBranch":"main",
		"prompt":"second",
		"libraryTaskName":"wf2",
		"agentHarness":"claude",
		"reviewers":["bob"],
		"images":[{"name":"a.png","mediaType":"image/png","dataBase64":"Zm9v"}]
	}`), "chat")

	task, ok := b.Task(requestID)
	if !ok {
		t.Fatal("Task() found = false, want true")
	}
	if task.Source != "chat" {
		t.Fatalf("task.Source = %q, want %q", task.Source, "chat")
	}
	if task.Prompt != "second" {
		t.Fatalf("task.Prompt = %q, want %q", task.Prompt, "second")
	}
	if task.Workflow != "wf2" {
		t.Fatalf("task.Workflow = %q, want %q", task.Workflow, "wf2")
	}
	if task.AgentHarness != "claude" {
		t.Fatalf("task.AgentHarness = %q, want %q", task.AgentHarness, "claude")
	}
	if task.Repo != "acme/repo2" {
		t.Fatalf("task.Repo = %q, want %q", task.Repo, "acme/repo2")
	}
	if len(task.Reviewers) != 1 || task.Reviewers[0] != "bob" {
		t.Fatalf("task.Reviewers = %#v, want [bob]", task.Reviewers)
	}
	if len(task.Images) != 1 || task.Images[0].Name != "a.png" {
		t.Fatalf("task.Images = %#v, want one image named a.png", task.Images)
	}
}

func TestRecordRejectedPromptSubmissionDefaultsStatusAndError(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := b.RecordRejectedPromptSubmissionWithSource(nil, "   ", nil, "")
	if requestID == "" {
		t.Fatal("RecordRejectedPromptSubmissionWithSource() returned empty request id")
	}
	task, ok := b.Task(requestID)
	if !ok {
		t.Fatal("Task() found = false, want true")
	}
	if task.Status != "invalid" {
		t.Fatalf("task.Status = %q, want %q", task.Status, "invalid")
	}
	if task.Error != "prompt submission failed" {
		t.Fatalf("task.Error = %q, want %q", task.Error, "prompt submission failed")
	}
}

// ---- dashboardStatsLocked edge cases ---------------------------------------

func TestDashboardStatsLockedHandlesMissingAndZeroTimeAttempts(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }

	b.mu.Lock()
	b.tasks["blank-id"] = &taskState{RequestID: "", Status: "running", StartedAt: now, UpdatedAt: now}
	b.tasks["req-direct"] = &taskState{RequestID: "req-direct", Status: "running", StartedAt: now, UpdatedAt: now}
	b.attempts["blank-attempt-root"] = []taskAttemptState{{RequestID: "", Status: "pending"}}
	b.attempts["zero-started-root"] = []taskAttemptState{{RequestID: "req-zero-started", Status: "completed", UpdatedAt: now}}
	b.attempts["zero-updated-root"] = []taskAttemptState{{RequestID: "req-zero-updated", Status: "completed", StartedAt: now}}
	b.mu.Unlock()

	snap := b.Snapshot()

	if snap.Stats.TotalTasks != 3 {
		t.Fatalf("stats.TotalTasks = %d, want 3 (nil task, blank id, and blank-request attempt must be skipped)", snap.Stats.TotalTasks)
	}
	if snap.Stats.ActiveTasks != 2 {
		t.Fatalf("stats.ActiveTasks = %d, want 2 (active accounting happens before the blank-request-id task is skipped)", snap.Stats.ActiveTasks)
	}
	if snap.Stats.CompletedTasks != 2 {
		t.Fatalf("stats.CompletedTasks = %d, want 2", snap.Stats.CompletedTasks)
	}
	if snap.Stats.MaxConcurrentTasks < 1 {
		t.Fatalf("stats.MaxConcurrentTasks = %d, want >= 1", snap.Stats.MaxConcurrentTasks)
	}
	if snap.Stats.AverageDurationSeconds != 0 {
		t.Fatalf("stats.AverageDurationSeconds = %f, want 0 (zero-time fallbacks must yield zero duration)", snap.Stats.AverageDurationSeconds)
	}
}

// ---- releasesSnapshotLocked tie-break --------------------------------------

func TestReleasesSnapshotLockedTieBreaksByRequestIDDescending(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }

	b.RecordReleaseFromTask(Task{RequestID: "req-a", PRURL: "https://github.com/acme/repo/pull/1"}, now.Format(time.RFC3339))
	b.RecordReleaseFromTask(Task{RequestID: "req-b", PRURL: "https://github.com/acme/repo/pull/2"}, now.Format(time.RFC3339))

	releases := b.Snapshot().Releases
	if len(releases) != 2 {
		t.Fatalf("len(releases) = %d, want 2", len(releases))
	}
	if releases[0].RequestID != "req-b" || releases[1].RequestID != "req-a" {
		t.Fatalf("releases order = [%s, %s], want [req-b, req-a] (tie-break by request id descending)", releases[0].RequestID, releases[1].RequestID)
	}
}

// ---- pure helper functions --------------------------------------------------

func TestTaskWorkflowAndTaskAgentHarnessHandleNilAndFallbacks(t *testing.T) {
	t.Parallel()

	if got := taskWorkflow(nil); got != "" {
		t.Fatalf("taskWorkflow(nil) = %q, want empty", got)
	}
	if got := taskAgentHarness(nil); got != "" {
		t.Fatalf("taskAgentHarness(nil) = %q, want empty", got)
	}
	if got := taskWorkflow(&taskState{Skill: "my-skill"}); got != "my-skill" {
		t.Fatalf("taskWorkflow(skill fallback) = %q, want %q", got, "my-skill")
	}
	if got := taskAgentHarness(&taskState{Stage: "codex"}); got != "codex" {
		t.Fatalf("taskAgentHarness(stage fallback) = %q, want %q", got, "codex")
	}
}

func TestLibraryTaskDisplayNameByNameReturnsEmptyForBlankName(t *testing.T) {
	t.Parallel()

	if got := libraryTaskDisplayNameByName("   "); got != "" {
		t.Fatalf("libraryTaskDisplayNameByName(blank) = %q, want empty", got)
	}
}

func TestIsDuplicateTaskLifecycleLineHandlesNilFields(t *testing.T) {
	t.Parallel()

	if isDuplicateTaskLifecycleLine(nil) {
		t.Fatal("isDuplicateTaskLifecycleLine(nil) = true, want false")
	}
	if !isDuplicateTaskLifecycleLine(map[string]string{"status": "duplicate"}) {
		t.Fatal("isDuplicateTaskLifecycleLine(status=duplicate) = false, want true")
	}
}

func TestClassifyEventKindRecognizesRuntimeLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line string
		want string
	}{
		{"task status=start request_id=req-1", "task_status"},
		{"dispatcher status=start cmd exec text=ls", "command_output"},
		{"localruntime.scheduler status=window cpu=1", "runtime"},
		{"some unrelated log line", "log"},
	}
	for _, tt := range tests {
		if got := classifyEventKind(tt.line); got != tt.want {
			t.Fatalf("classifyEventKind(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestNormalizeTaskTerminalStatusMapsLegacyOK(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"ok", "completed"},
		{"  ok  ", "completed"},
		{"running", "running"},
		{"", ""},
		{"  ", ""},
	}
	for _, tt := range tests {
		if got := normalizeTaskTerminalStatus(tt.in); got != tt.want {
			t.Fatalf("normalizeTaskTerminalStatus(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestErrorTextHandlesNilError(t *testing.T) {
	t.Parallel()

	if got := errorText(nil); got != "" {
		t.Fatalf("errorText(nil) = %q, want empty", got)
	}
	if got := errorText(errors.New("boom")); got != "boom" {
		t.Fatalf("errorText(err) = %q, want %q", got, "boom")
	}
}

func TestFirstRepoSkipsBlankEntries(t *testing.T) {
	t.Parallel()

	if got := firstRepo(nil); got != "" {
		t.Fatalf("firstRepo(nil) = %q, want empty", got)
	}
	if got := firstRepo([]string{"  ", ""}); got != "" {
		t.Fatalf("firstRepo(all blank) = %q, want empty", got)
	}
	if got := firstRepo([]string{"", " ", "acme/repo"}); got != "acme/repo" {
		t.Fatalf("firstRepo() = %q, want %q", got, "acme/repo")
	}
}

func TestSameStringSliceComparesElementwise(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{"equal", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"same length different values", []string{"a", "b"}, []string{"a", "c"}, false},
		{"both nil", nil, nil, true},
	}
	for _, tt := range tests {
		if got := sameStringSlice(tt.a, tt.b); got != tt.want {
			t.Fatalf("%s: sameStringSlice(%v, %v) = %t, want %t", tt.name, tt.a, tt.b, got, tt.want)
		}
	}
}

func TestSamePromptImagesComparesElementwise(t *testing.T) {
	t.Parallel()

	imgA := PromptImage{Name: "a.png", MediaType: "image/png", DataBase64: "AAAA"}
	imgB := PromptImage{Name: "b.png", MediaType: "image/png", DataBase64: "BBBB"}

	tests := []struct {
		name string
		a, b []PromptImage
		want bool
	}{
		{"equal", []PromptImage{imgA}, []PromptImage{imgA}, true},
		{"different length", []PromptImage{imgA}, []PromptImage{imgA, imgB}, false},
		{"same length different values", []PromptImage{imgA}, []PromptImage{imgB}, false},
	}
	for _, tt := range tests {
		if got := samePromptImages(tt.a, tt.b); got != tt.want {
			t.Fatalf("%s: samePromptImages() = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestNormalizeHTTPURLRejectsNonHTTPSchemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"http://example.com", "http://example.com"},
		{"  https://example.com  ", "https://example.com"},
		{"ftp://example.com", ""},
		{"not a url", ""},
	}
	for _, tt := range tests {
		if got := normalizeHTTPURL(tt.in); got != tt.want {
			t.Fatalf("normalizeHTTPURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWorkflowFromFieldsAndAgentHarnessFromFieldsHandleNil(t *testing.T) {
	t.Parallel()

	if got := workflowFromFields(nil); got != "" {
		t.Fatalf("workflowFromFields(nil) = %q, want empty", got)
	}
	if got := agentHarnessFromFields(nil); got != "" {
		t.Fatalf("agentHarnessFromFields(nil) = %q, want empty", got)
	}
	if got := workflowFromFields(map[string]string{"skill": "my-skill"}); got != "my-skill" {
		t.Fatalf("workflowFromFields(skill fallback) = %q, want %q", got, "my-skill")
	}
	if got := agentHarnessFromFields(map[string]string{"stage": "claude"}); got != "claude" {
		t.Fatalf("agentHarnessFromFields(stage fallback) = %q, want %q", got, "claude")
	}
}

func TestTaskPRURLFromFieldsAndTaskLiveAppURLFromFieldsHandleNilAndFallbacks(t *testing.T) {
	t.Parallel()

	if got := taskPRURLFromFields(nil); got != "" {
		t.Fatalf("taskPRURLFromFields(nil) = %q, want empty", got)
	}
	if got := taskPRURLFromFields(map[string]string{"pr_urls": "https://github.com/acme/repo/pull/1,https://github.com/acme/repo/pull/2"}); got != "https://github.com/acme/repo/pull/1" {
		t.Fatalf("taskPRURLFromFields(pr_urls) = %q, want first entry", got)
	}

	if got := taskLiveAppURLFromFields(nil); got != "" {
		t.Fatalf("taskLiveAppURLFromFields(nil) = %q, want empty", got)
	}
	if got := taskLiveAppURLFromFields(map[string]string{}); got != "" {
		t.Fatalf("taskLiveAppURLFromFields(empty) = %q, want empty", got)
	}
	if got := taskLiveAppURLFromFields(map[string]string{"event": "preview_ready", "url": "https://preview.example.com"}); got != "https://preview.example.com" {
		t.Fatalf("taskLiveAppURLFromFields(event-based) = %q, want %q", got, "https://preview.example.com")
	}
}

func TestRunConfigJSONHelpersHandleEmptyAndMalformedPayloads(t *testing.T) {
	t.Parallel()

	if got := reposFromRunConfigJSON(nil); got != nil {
		t.Fatalf("reposFromRunConfigJSON(nil) = %v, want nil", got)
	}
	if got := reposFromRunConfigJSON([]byte("{")); got != nil {
		t.Fatalf("reposFromRunConfigJSON(malformed) = %v, want nil", got)
	}

	if got := reviewersFromRunConfigJSON(nil); got != nil {
		t.Fatalf("reviewersFromRunConfigJSON(nil) = %v, want nil", got)
	}
	if got := reviewersFromRunConfigJSON([]byte("{")); got != nil {
		t.Fatalf("reviewersFromRunConfigJSON(malformed) = %v, want nil", got)
	}

	if got := workflowFromRunConfigJSON(nil); got != "" {
		t.Fatalf("workflowFromRunConfigJSON(nil) = %q, want empty", got)
	}
	if got := workflowFromRunConfigJSON([]byte("{")); got != "" {
		t.Fatalf("workflowFromRunConfigJSON(malformed) = %q, want empty", got)
	}

	if got := agentHarnessFromRunConfigJSON(nil); got != "" {
		t.Fatalf("agentHarnessFromRunConfigJSON(nil) = %q, want empty", got)
	}
	if got := agentHarnessFromRunConfigJSON([]byte("{")); got != "" {
		t.Fatalf("agentHarnessFromRunConfigJSON(malformed) = %q, want empty", got)
	}

	if got := sourceFromRunConfigJSON(nil); got != "" {
		t.Fatalf("sourceFromRunConfigJSON(nil) = %q, want empty", got)
	}
	if got := sourceFromRunConfigJSON([]byte("{")); got != "" {
		t.Fatalf("sourceFromRunConfigJSON(malformed) = %q, want empty", got)
	}
}

func TestParseIntFieldAndParseFloatField(t *testing.T) {
	t.Parallel()

	intTests := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"", 0, false},
		{"   ", 0, false},
		{"abc", 0, false},
		{"42", 42, true},
		{" 7 ", 7, true},
	}
	for _, tt := range intTests {
		got, ok := parseIntField(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Fatalf("parseIntField(%q) = (%d, %t), want (%d, %t)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}

	floatTests := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"", 0, false},
		{"   ", 0, false},
		{"abc", 0, false},
		{"12.5", 12.5, true},
		{" 3 ", 3, true},
	}
	for _, tt := range floatTests {
		got, ok := parseFloatField(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Fatalf("parseFloatField(%q) = (%f, %t), want (%f, %t)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestParseKVFieldsHandlesTrailingSpacesAndUnquoteFallback(t *testing.T) {
	t.Parallel()

	fields := parseKVFields("status=start   ")
	if fields["status"] != "start" {
		t.Fatalf("parseKVFields(trailing spaces) = %#v, want status=start", fields)
	}

	// A token without '=' is skipped; parsing resumes at the next field.
	fields = parseKVFields("status start=1")
	if fields["start"] != "1" {
		t.Fatalf("parseKVFields(malformed leading token) = %#v, want start=1", fields)
	}

	// A quoted value with an invalid escape sequence falls back to a plain
	// quote-trim instead of propagating the strconv.Unquote error.
	fields = parseKVFields(`key="\q" status=ok`)
	if fields["key"] != `\q` {
		t.Fatalf(`parseKVFields(invalid escape) = %#v, want key=\q`, fields)
	}
	if fields["status"] != "ok" {
		t.Fatalf("parseKVFields(invalid escape) = %#v, want status=ok to still parse", fields)
	}
}

func TestParseQuotedTokenRequiresLeadingAndClosingQuote(t *testing.T) {
	t.Parallel()

	if _, ok := parseQuotedToken("no leading quote"); ok {
		t.Fatal("parseQuotedToken(no leading quote) ok = true, want false")
	}
	if _, ok := parseQuotedToken(`"unterminated`); ok {
		t.Fatal("parseQuotedToken(unterminated) ok = true, want false")
	}
	if token, ok := parseQuotedToken(`"hello" trailing`); !ok || token != `"hello"` {
		t.Fatalf("parseQuotedToken(valid) = (%q, %t), want (%q, true)", token, ok, `"hello"`)
	}
}

func TestSortedCountStatsGroupsSkipsNilAndBreaksTiesByName(t *testing.T) {
	t.Parallel()

	groups := map[string]*CountStatsGroup{
		"nil-entry": nil,
		"low":       {Name: "low", Tasks: 1},
		"high":      {Name: "high", Tasks: 5},
	}
	out := sortedCountStatsGroups(groups)
	if len(out) != 2 {
		t.Fatalf("len(sortedCountStatsGroups()) = %d, want 2 (nil entry skipped)", len(out))
	}
	if out[0].Name != "high" || out[1].Name != "low" {
		t.Fatalf("sortedCountStatsGroups() order = [%s, %s], want [high, low]", out[0].Name, out[1].Name)
	}
}
