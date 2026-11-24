package web

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestConfiguredIssueAutomationCoexistsWithBuilder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		feature, custom bool
		want            int
	}{
		{"disabled feature preserves default", false, true, 1},
		{"draft preserves default", true, false, 1},
		{"enabled custom owns issue family", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, f := recoveryServer(t)
			store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
			w := workflow.Definition{ID: "custom", Name: "Custom", Repository: "acme/repo", Enabled: tc.custom, Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", Instructions: "Implement issue", IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash"}}
			if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
				t.Fatal(err)
			}
			s.Workflows = &workflow.Engine{Store: store, Enabled: func() bool { return tc.feature }, Allowed: func(string) bool { return true }}
			s.pollGitHubIssueAutoStart(context.Background())
			if len(f.submissions) != tc.want {
				t.Fatalf("configured submissions = %d, want %d", len(f.submissions), tc.want)
			}
		})
	}
}

func TestPRMonitorDoesNotDeleteCustomWorkflowBranch(t *testing.T) {
	t.Parallel()
	broker := NewBroker()
	broker.IngestLog("dispatch status=start request_id=custom source=workflow repo=git@github.com:acme/repo.git")
	broker.IngestLog("dispatch status=completed request_id=custom branch=agent00-custom pr_url=https://github.com/acme/repo/pull/42")
	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"MERGED","mergedAt":"2026-04-09T12:00:00Z","headRefName":"agent00-custom","baseRefName":"main"}`}}
	monitor := PRMergeMonitor{Runner: runner, Broker: broker, Logf: func(string, ...any) {}, DeleteMergedBranches: true, DefaultAutomationEnabled: func(Task) bool { return false }}
	monitor.checkTaskPR(context.Background(), broker.Snapshot().Tasks[0])
	if len(runner.Commands()) != 1 {
		t.Fatal("default monitor mutated custom workflow branch")
	}
}
