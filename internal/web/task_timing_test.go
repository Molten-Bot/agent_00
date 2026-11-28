package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/tasktiming"
)

func TestBrokerRetainsTimingBeforeTaskStart(t *testing.T) {
	t.Parallel()
	t.Run("lifecycle log", func(t *testing.T) { testBrokerEarlyTiming(t, false) })
	t.Run("run config", func(t *testing.T) { testBrokerEarlyTiming(t, true) })
}

func testBrokerEarlyTiming(t *testing.T, registerConfig bool) {
	t.Helper()
	b := NewBroker()
	b.maxEvents = 1
	acceptedAt := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	now := acceptedAt
	b.now = func() time.Time { return now }
	const queueStart = "task_timing request_id=delivery attempt=first span=1 stage=queue started_at=2026-11-27T12:00:00Z ended_at=unknown wall_ms=unknown agent_ms=unknown"
	const queueEnd = "task_timing request_id=delivery attempt=first span=1 stage=queue started_at=2026-11-27T12:00:00Z ended_at=2026-11-27T12:00:02Z wall_ms=2000 agent_ms=unknown"
	const taskStart = "task status=start request_id=delivery"
	b.IngestLog(queueStart)
	if _, ok := b.Task("delivery"); ok {
		t.Fatal("timing created a task before its lifecycle log")
	}
	now = now.Add(2 * time.Second)
	b.IngestLog(queueEnd)
	if registerConfig {
		b.RecordTaskRunConfig("delivery", []byte(`{"prompt":"fixture"}`))
	}
	b.IngestLog(taskStart)
	task, ok := b.Task("delivery")
	if !ok || task.TimingRevision != 2 || task.TimingAttempt != "first" {
		t.Fatalf("task timing state = %+v, found = %v", task, ok)
	}
	want := []TaskLog{
		{Time: acceptedAt.Format(time.RFC3339Nano), Stream: "meta", Text: queueStart},
		{Time: now.Format(time.RFC3339Nano), Stream: "meta", Text: queueEnd},
		{Time: now.Format(time.RFC3339Nano), Stream: "meta", Text: taskStart},
	}
	if !slices.Equal(task.Logs, want) {
		t.Fatalf("task logs = %+v, want %+v", task.Logs, want)
	}
}

func TestTaskTimingHistoryInBrowser(t *testing.T) {
	requireBrowserTooling(t, "task timing history")
	broker := NewBroker()
	broker.IngestLog("task status=start request_id=delivery")
	broker.IngestLog("task_timing request_id=delivery attempt=retry span=queue stage=queue started_at=2026-11-27T12:00:10Z ended_at=unknown wall_ms=unknown agent_ms=unknown")
	broker.IngestLog("task status=completed request_id=delivery")
	broker.IngestLog("workflow event=transition workflow_id=delivery request_id=delivery status=merged reason=merged duration_ms=60000")

	const taskLog = `task_timing request_id=delivery-first attempt=first span=1 stage=implementation started_at=2026-11-27T12:00:00Z ended_at=2026-11-27T12:00:05Z wall_ms=5000 agent_ms=3000
task_timing request_id=delivery attempt=first span=wait stage=workflow_wait started_at=2026-11-27T12:00:05Z ended_at=2026-11-27T12:00:10Z wall_ms=5000 agent_ms=unknown
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
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("task timing API status = %d", response.StatusCode)
	}
	var spans []tasktiming.Span
	if err := json.NewDecoder(response.Body).Decode(&spans); err != nil {
		t.Fatal(err)
	}
	if len(spans) != 4 || spans[0].Attempt != "first" || spans[0].Stage != tasktiming.WorkflowWait ||
		spans[0].WallMS == nil || *spans[0].WallMS != 5000 || spans[3].WallMS == nil || *spans[3].WallMS != 60000 {
		t.Fatalf("task timing API omitted previous wait or terminal total: %+v", spans)
	}
	runBrowserSuite(t, testServer.URL, time.Minute, "task timing history", "tests/task_timing.cjs")
}
