package main

import (
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
)

func TestTaskReviewPassesRequiresWorkflow(t *testing.T) {
	t.Parallel()

	if got := taskReviewPasses(config.Config{}); got != 0 {
		t.Fatalf("manual task review passes = %d, want 0", got)
	}
	for _, count := range []int{0, 2, 5, 10} {
		run := config.Config{Workflow: &config.WorkflowRunConfig{ReviewCycles: count}}
		if got := taskReviewPasses(run); got != count {
			t.Errorf("workflow task review passes = %d, want %d", got, count)
		}
	}
}
