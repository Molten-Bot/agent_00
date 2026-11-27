package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/tasktiming"
)

func TestTaskTimingHistoryInBrowser(t *testing.T) {
	requireBrowserTooling(t, "task timing history")
	broker := NewBroker()
	broker.IngestLog("task status=start request_id=delivery")
	broker.IngestLog("task status=completed request_id=delivery")
	broker.IngestLog("workflow event=transition workflow_id=delivery request_id=delivery status=merged reason=merged duration_ms=60000")

	const taskLog = `task_timing request_id=delivery-first attempt=first span=1 stage=implementation started_at=2026-11-27T12:00:00Z ended_at=2026-11-27T12:00:05Z wall_ms=5000 agent_ms=3000
task_timing request_id=delivery attempt=retry span=1 stage=ci_wait started_at=2026-11-27T12:00:10Z ended_at=2026-11-27T12:00:30Z wall_ms=20000 agent_ms=unknown
task_timing request_id=delivery attempt=retry span=2 stage=ready started_at=unknown ended_at=2026-11-27T12:00:38Z wall_ms=unknown agent_ms=unknown
task_timing request_id=delivery attempt=retry span=3 stage=workflow_total started_at=2026-11-27T12:00:00Z ended_at=2026-11-27T12:01:00Z wall_ms=60000 agent_ms=unknown
`
	server := NewServer("", broker)
	server.IssueClaims = &localruntime.IssueClaimStore{Root: t.TempDir()}
	for i, requestID := range []string{"delivery-first", "delivery"} {
		id := strings.Repeat(string(rune('1'+i)), 32)
		record := localruntime.IssueClaimRecord{
			ID: id, Version: 2, RequestID: requestID, Phase: "submitted", State: "fulfilled", Outcome: "completed",
			Run: config.Config{RepoURL: "https://github.com/acme/repo", GitHubIssue: &config.GitHubIssueConfig{
				Repository: "acme/repo", Number: 1, Claim: &config.GitHubIssueClaim{ID: id, Attempt: i + 1},
			}},
		}
		if err := server.IssueClaims.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	server.LoadTaskTiming = func(ctx context.Context, requestID string) ([]tasktiming.Span, error) {
		return tasktiming.Read(ctx, strings.NewReader(taskLog), requestID)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()

	response, err := testServer.Client().Get(testServer.URL + "/api/tasks/delivery/timing")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("task timing API status = %d", response.StatusCode)
	}
	runBrowserSuite(t, testServer.URL, time.Minute, "task timing history", "tests/task_timing.cjs")
}
