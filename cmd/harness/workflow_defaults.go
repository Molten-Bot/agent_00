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

// Earlier builder versions copied schedules without removing config.json's
// originals. Never run both copies, or revive a paused/edited custom copy.
// An unchanged enabled copy can fall back to its configured default when the
// builder feature is off, so the feature toggle does not disable old schedules.
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
	sum := sha256.Sum256([]byte(schedule.Key))
	id := "library-" + hex.EncodeToString(sum[:12])
	for _, w := range doc.Workflows {
		if w.ID != id {
			continue
		}
		if !w.Enabled || (engine.Enabled != nil && engine.Enabled()) {
			return true
		}
		target := strings.TrimSpace(schedule.TargetSubdir)
		if target == "" {
			target = "."
		}
		reviewers := append([]string{}, schedule.Reviewers...)
		return workflow.CanonicalRepository(w.Repository) != workflow.CanonicalRepository(schedule.Repo) ||
			w.Trigger.Event != "schedule" || w.Trigger.IntervalHours != float64(schedule.IntervalHours) ||
			w.Task.Kind != "implement" || w.Task.LibraryTaskName != schedule.LibraryTaskName ||
			w.Task.Instructions != schedule.PromptContext || w.Task.BaseBranch != schedule.BaseBranch ||
			w.Task.TargetSubdir != target || w.Task.Draft != schedule.Draft ||
			w.Task.Reviewers == nil || !reflect.DeepEqual(*w.Task.Reviewers, reviewers) ||
			w.ReviewCycles != 0 || w.Merge != (workflow.Merge{Method: "squash", RequireApproval: true})
	}
	return false
}
