package main

import (
	"encoding/json"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
)

// Include manual runs and queued/paused work even when their branch or current
// library prompt differs from the saved schedule.
func libraryScheduleActive(broker *web.Broker, schedule localruntime.LibrarySchedule) bool {
	for _, task := range broker.Snapshot().Tasks {
		switch task.Status {
		case "pending", "running", "paused":
		default:
			continue
		}
		body, ok := broker.TaskRunConfig(task.RequestID)
		if !ok {
			continue
		}
		var runCfg config.Config
		if json.Unmarshal(body, &runCfg) != nil {
			continue
		}
		for _, repo := range runCfg.RepoList() {
			key, err := localruntime.LibraryScheduleKey(runCfg.LibraryTaskName, repo)
			if err == nil && key == schedule.Key {
				return true
			}
		}
	}
	return false
}
