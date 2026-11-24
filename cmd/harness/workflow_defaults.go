package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func taskReviewPasses(run config.Config, defaults localruntime.ReviewWatchConfig) int {
	if run.Workflow != nil {
		return run.Workflow.ReviewCycles
	}
	return defaults.ReviewCount()
}

func taskUsesDefaultPRAutomation(broker *web.Broker, engine *workflow.Engine, task web.Task) bool {
	if task.Source == "workflow" {
		return false
	}
	if raw, ok := broker.TaskRunConfig(task.RequestID); ok {
		var run config.Config
		if json.Unmarshal(raw, &run) != nil || run.Workflow != nil {
			return false
		}
	}
	return !engine.Owns(task.Repo, "pr.changes_requested") && !engine.Owns(task.Repo, "pr.checks_failed")
}

// Earlier builder versions copied schedules without removing their originals.
// Unchanged imported copies keep the configured scheduler as their single owner.
// Paused or edited copies retain explicit custom ownership.
func workflowOwnsLibrarySchedule(engine *workflow.Engine, schedule localruntime.LibrarySchedule) bool {
	if engine == nil {
		return false
	}
	if engine.Store == nil {
		return true
	}
	doc, err := engine.Store.Load()
	if err != nil {
		return true
	}
	if doc.ConfigurationMigrated {
		return true
	}
	for _, w := range doc.Workflows {
		if w.ID == importedScheduleID(schedule) {
			return !w.Enabled || !unchangedImportedSchedule(w, schedule)
		}
	}
	return false
}

func importedScheduleID(schedule localruntime.LibrarySchedule) string {
	sum := sha256.Sum256([]byte(schedule.Key))
	return "library-" + hex.EncodeToString(sum[:12])
}

func unchangedImportedSchedule(w workflow.Definition, schedule localruntime.LibrarySchedule) bool {
	target := strings.TrimSpace(schedule.TargetSubdir)
	if target == "" {
		target = "."
	}
	reviewers := append([]string{}, schedule.Reviewers...)
	return w.ID == importedScheduleID(schedule) &&
		workflow.CanonicalRepository(w.Repository) == workflow.CanonicalRepository(schedule.Repo) &&
		w.Trigger.Event == "schedule" && w.Trigger.IntervalHours == float64(schedule.IntervalHours) &&
		w.Task.Kind == "implement" && w.Task.LibraryTaskName == schedule.LibraryTaskName &&
		w.Task.Instructions == schedule.PromptContext && w.Task.BaseBranch == schedule.BaseBranch &&
		w.Task.TargetSubdir == target && w.Task.Draft == schedule.Draft &&
		w.Task.Reviewers != nil && reflect.DeepEqual(*w.Task.Reviewers, reviewers) &&
		w.ReviewCycles == 0 && w.Merge == (workflow.Merge{Method: "squash", RequireApproval: true})
}

func configuredScheduleOwnsWorkflow(path string, w workflow.Definition) bool {
	if !strings.HasPrefix(w.ID, "library-") {
		return false
	}
	schedules, err := (&localruntime.LibrarySchedules{Path: path}).List()
	if err != nil {
		return true
	}
	for _, schedule := range schedules {
		if unchangedImportedSchedule(w, schedule) {
			return true
		}
	}
	return false
}

func deferConfiguredLibrarySchedule(engine *workflow.Engine, path string, schedule localruntime.LibrarySchedule) bool {
	if workflowOwnsLibrarySchedule(engine, schedule) {
		return true
	}
	if engine == nil {
		return false
	}
	// Read the current journal whenever admission is due, not only at startup:
	// an edited custom copy may have run before being restored to its original.
	statuses, err := engine.ScheduleStatus()
	if err != nil {
		return true
	}
	for _, status := range statuses {
		if status.WorkflowID == importedScheduleID(schedule) && status.NextRunAt.After(schedule.NextRunAt) {
			// Advance through a separate store facade: the caller holds its own
			// scheduler mutex, while both stores share the config transaction lock.
			_ = (&localruntime.LibrarySchedules{Path: path}).AdvanceNextRunAt(schedule.Key, status.NextRunAt)
			return true // A failed persistence must also prevent duplicate work.
		}
	}
	return false
}
