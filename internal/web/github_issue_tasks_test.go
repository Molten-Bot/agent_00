package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestIssueTaskDeleteGenerationSurvivesBrokerRestart(t *testing.T) {
	t.Parallel()

	s := NewServer("", NewBroker())
	s.IssueClaims = &localruntime.IssueClaimStore{Root: t.TempDir()}
	cleanups := 0
	s.CloseTask = func(_ context.Context, id string) error {
		cleanups++
		s.Broker.IngestLog("task status=ok action=task_close_cleanup request_id=" + id)
		return nil
	}
	record := localruntime.IssueClaimRecord{
		Version: 2, ID: "11111111111111111111111111111111", RequestID: "issue-task",
		Phase: "submitted", State: "fulfilled", Outcome: "completed",
		Run: config.Config{
			RepoURL: "https://github.com/acme/repo",
			GitHubIssue: &config.GitHubIssueConfig{
				Repository: "acme/repo", Number: 1,
				Claim: &config.GitHubIssueClaim{ID: "11111111111111111111111111111111", Attempt: 1},
			},
		},
	}
	publishResult := func() {
		t.Helper()
		if err := s.IssueClaims.Save(record); err != nil {
			t.Fatal(err)
		}
		run, err := json.Marshal(record.Run)
		if err != nil {
			t.Fatal(err)
		}
		s.Broker.RecordTaskRunConfig(record.RequestID, run)
		s.Broker.IngestLog("task status=completed request_id=" + record.RequestID)
	}
	readTasks := func() []Task {
		t.Helper()
		resp := httptest.NewRecorder()
		s.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var snapshot Snapshot
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot.Tasks
	}
	readTask := func() Task {
		t.Helper()
		tasks := readTasks()
		if len(tasks) != 1 || tasks[0].RequestID != record.RequestID {
			t.Fatalf("expected one issue task, got %+v", tasks)
		}
		return tasks[0]
	}
	deleteTask := func(generations map[string]string, want int) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"attempt_generations": generations})
		if err != nil {
			t.Fatal(err)
		}
		resp := httptest.NewRecorder()
		s.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/tasks/"+record.RequestID+"/delete", strings.NewReader(string(body))))
		if resp.Code != want {
			t.Fatalf("delete status = %d, want %d: %s", resp.Code, want, resp.Body.String())
		}
	}

	publishResult()
	before := readTask()
	generation := before.DeleteScope[record.RequestID]
	if generation == "" {
		t.Fatal("live result has no deletion generation")
	}
	liveBroker := s.Broker
	s.Broker = NewBroker()
	s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	restored := readTask()
	if got := restored.DeleteScope[record.RequestID]; !restored.HistoryOnly || got != generation {
		t.Fatalf("restored history-only = %v, generation = %q, want history-only and generation %q", restored.HistoryOnly, got, generation)
	}
	deleteTask(map[string]string{record.RequestID: "stale"}, http.StatusNotFound)
	readTask()
	deleteTask(restored.DeleteScope, http.StatusOK)
	if tasks := readTasks(); len(tasks) != 0 {
		t.Fatalf("deleted issue task still appears in status: %+v", tasks)
	}
	deleteTask(restored.DeleteScope, http.StatusNotFound)
	restartedBroker := s.Broker

	// Draft verification can use a new claim with the original request ID.
	s.Broker = liveBroker
	s.Broker.IngestLog("task status=queued request_id=" + record.RequestID + " action=resume_draft_verification")
	record.ID = "22222222222222222222222222222222"
	record.Run.GitHubIssue.Claim = &config.GitHubIssueClaim{ID: record.ID, Attempt: 2}
	publishResult()
	if got := readTask().DeleteScope[record.RequestID]; got == "" || got == generation {
		t.Fatalf("new claim generation = %q, previous generation = %q", got, generation)
	}
	deleteTask(before.DeleteScope, http.StatusNotFound)
	readTask()
	s.Broker = restartedBroker
	deleteTask(before.DeleteScope, http.StatusNotFound)
	readTask()
	if cleanups != 1 {
		t.Fatalf("expected cleanup for only the matching result, got %d calls", cleanups)
	}
}
