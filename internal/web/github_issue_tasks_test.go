package web

import (
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
	readTask := func() Task {
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
		if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].RequestID != record.RequestID {
			t.Fatalf("expected one issue task, got %+v", snapshot.Tasks)
		}
		return snapshot.Tasks[0]
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

	// Draft verification can use a new claim with the original request ID.
	s.Broker = liveBroker
	s.Broker.IngestLog("task status=queued request_id=" + record.RequestID + " action=resume_draft_verification")
	record.ID = "22222222222222222222222222222222"
	record.Run.GitHubIssue.Claim = &config.GitHubIssueClaim{ID: record.ID, Attempt: 2}
	publishResult()
	if got := readTask().DeleteScope[record.RequestID]; got == "" || got == generation {
		t.Fatalf("new claim generation = %q, previous generation = %q", got, generation)
	}
	body, err := json.Marshal(map[string]any{"attempt_generations": before.DeleteScope})
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	s.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/tasks/"+record.RequestID+"/delete", strings.NewReader(string(body))))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("stale delete status = %d: %s", resp.Code, resp.Body.String())
	}
	readTask()
}
