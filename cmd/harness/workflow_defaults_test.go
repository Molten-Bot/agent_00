package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestTaskReviewPassesKeepConfiguredDefaults(t *testing.T) {
	t.Parallel()
	defaults := localruntime.ReviewWatchConfig{ReviewLevel: "medium"}
	if got := taskReviewPasses(config.Config{}, defaults); got != 5 {
		t.Fatalf("configured reviews = %d, want 5", got)
	}
	for _, count := range []int{0, 2} {
		if got := taskReviewPasses(config.Config{Workflow: &config.WorkflowRunConfig{ReviewCycles: count}}, defaults); got != count {
			t.Fatalf("explicit review override = %d, want %d", got, count)
		}
	}
}

func TestLegacyScheduleOwnershipAfterBuilderMigration(t *testing.T) {
	t.Parallel()
	schedule := localruntime.LibrarySchedule{LibraryScheduleInput: localruntime.LibraryScheduleInput{LibraryTaskName: "unit-test-coverage", Repo: "https://github.com/acme/repo.git", IntervalHours: 24}, Key: "unit-test-coverage@acme/repo", NextRunAt: time.Now()}
	for _, tc := range []struct {
		name                                string
		feature, enabled, edited, wantOwned bool
	}{
		{"unchanged default feature off", false, true, false, false},
		{"configured owner even with builder active", true, true, false, false},
		{"paused remains paused", false, false, false, true},
		{"edited remains custom", false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sum := sha256.Sum256([]byte(schedule.Key))
			reviewers := []string{}
			w := workflow.Definition{ID: "library-" + hex.EncodeToString(sum[:12]), Name: "Imported", Repository: "acme/repo", Enabled: tc.enabled, Trigger: workflow.Trigger{Event: "schedule", IntervalHours: 24}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", LibraryTaskName: schedule.LibraryTaskName, TargetSubdir: ".", Reviewers: &reviewers, IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash", RequireApproval: true}}
			if tc.edited {
				w.Task.Instructions = "custom change"
			}
			store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
			if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
				t.Fatal(err)
			}
			e := &workflow.Engine{Store: store, Enabled: func() bool { return tc.feature }}
			if got := workflowOwnsLibrarySchedule(e, schedule); got != tc.wantOwned {
				t.Fatalf("custom ownership = %v, want %v", got, tc.wantOwned)
			}
		})
	}
}

func TestImportedScheduleHasOneCursorAcrossFeatureTogglesAndEdits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(root, "config.json")
	legacy := &localruntime.LibrarySchedules{Path: path, Now: func() time.Time { return now }}
	schedule, err := legacy.Save(localruntime.LibraryScheduleInput{LibraryTaskName: "unit-test-coverage", Repo: "acme/repo", IntervalHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	reviewers := []string{}
	w := workflow.Definition{ID: importedScheduleID(schedule), Name: "Imported", Repository: "acme/repo", Enabled: true, Trigger: workflow.Trigger{Event: "schedule", IntervalHours: 24, StartAt: &schedule.NextRunAt}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", LibraryTaskName: schedule.LibraryTaskName, TargetSubdir: ".", Reviewers: &reviewers, IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash", RequireApproval: true}}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	enabled, customRuns, defaultRuns := true, 0, 0
	e := &workflow.Engine{Store: store, StatePath: filepath.Join(root, "state.json"), Now: func() time.Time { return now }, Enabled: func() bool { return enabled }, Allowed: func(string) bool { return true }, Submit: func(context.Context, workflow.Run) (string, error) { customRuns++; return "custom", nil }, Status: func(string) workflow.Outcome { return workflow.Outcome{Status: "succeeded"} }}
	tick := func() {
		t.Helper()
		if err := e.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	now = schedule.NextRunAt
	tick() // Simulate an occurrence already reserved by the older builder.
	tick()
	if customRuns != 1 {
		t.Fatal(customRuns)
	}
	e.Active = func(w workflow.Definition) bool { return configuredScheduleOwnsWorkflow(path, w) }
	legacy.Active = func(s localruntime.LibrarySchedule) bool { return deferConfiguredLibrarySchedule(e, path, s) }
	legacy.Submit = func(context.Context, config.Config) (string, error) { defaultRuns++; return "default", nil }
	runDefault := func() {
		t.Helper()
		if err := legacy.RunDue(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, feature := range []bool{false, true, false} {
		enabled = feature
		runDefault()
		tick()
	}
	if defaultRuns != 0 || customRuns != 1 {
		t.Fatalf("replayed imported occurrence: default=%d custom=%d", defaultRuns, customRuns)
	}
	now = now.Add(24 * time.Hour)
	enabled = true
	runDefault()
	tick()
	if defaultRuns != 1 || customRuns != 1 {
		t.Fatalf("single owner violated: default=%d custom=%d", defaultRuns, customRuns)
	}
	// Editing makes the copy custom-owned; reverting it must adopt its latest
	// reservation even without restarting the application.
	doc, _ := store.Load()
	doc.Workflows[0].Task.Instructions = "custom instructions"
	if _, err := store.Save(doc); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	tick()
	tick()
	doc, _ = store.Load()
	doc.Workflows[0].Task.Instructions = ""
	if _, err := store.Save(doc); err != nil {
		t.Fatal(err)
	}
	runDefault()
	if defaultRuns != 1 || customRuns != 2 {
		t.Fatalf("replayed restored custom occurrence: default=%d custom=%d", defaultRuns, customRuns)
	}
}

func TestDefaultPRMonitorExcludesExplicitWorkflowTasks(t *testing.T) {
	t.Parallel()
	broker := web.NewBroker()
	if !taskUsesDefaultPRAutomation(broker, nil, web.Task{Source: "local"}) {
		t.Fatal("configured default monitor disabled")
	}
	if taskUsesDefaultPRAutomation(broker, nil, web.Task{Source: "workflow"}) {
		t.Fatal("default monitor took ownership of custom task")
	}
}
