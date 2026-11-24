package main

import (
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
		{"custom runner active", true, true, false, true},
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
