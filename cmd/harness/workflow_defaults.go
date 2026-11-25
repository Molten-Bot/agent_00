package main

import "github.com/Molten-Bot/agent_00/internal/config"

func taskReviewPasses(run config.Config) int {
	if run.Workflow == nil {
		return 0
	}
	return run.Workflow.ReviewCycles
}
