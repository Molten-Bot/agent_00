package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func inputWorkflowServer(t *testing.T, source string) (Server, workflow.Definition) {
	t.Helper()
	root := t.TempDir()
	w := workflow.Definition{ID: "input", Name: "Input", Enabled: true, Repository: "acme/repo", Trigger: workflow.Trigger{Event: "app." + source}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", Instructions: "Run focused tests", IssueLinkMode: "reference"}, ReviewCycles: 2, Merge: workflow.Merge{Method: "squash", RequireApproval: true}}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("", NewBroker())
	s.Workflows = &workflow.Engine{Store: store, StatePath: filepath.Join(root, "state.json"), Enabled: func() bool { return true }, Allowed: func(string) bool { return true }, Fetch: func(context.Context, string) ([]workflow.Item, error) {
		t.Fatal("app input polled GitHub")
		return nil, nil
	}, Status: func(string) workflow.Outcome { return workflow.Outcome{Status: "running"} }}
	return s, w
}

func TestInputWorkflowsKeepPayloadAndJournalCompletion(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"prompt", "json", "chat"} {
		t.Run(source, func(t *testing.T) {
			s, _ := inputWorkflowServer(t, source)
			body := []byte("{\n\"repoUrl\":\"https://github.com/acme/repo.git\",\"prompt\":\"Fix the endpoint\",\"baseBranch\":\"feature\",\"targetSubdir\":\"src\",\"responseMode\":\"off\"}")
			var got config.Config
			id, err := s.submitInputWorkflow(t.Context(), body, source, func(_ context.Context, data []byte) (string, error) {
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				return "request-one", nil
			})
			if err != nil || id != "request-one" {
				t.Fatal(id, err)
			}
			if got.Workflow == nil || got.Workflow.ReviewCycles != 2 || !strings.Contains(got.Prompt, "Fix the endpoint") || !strings.Contains(got.Prompt, "Run focused tests") || got.BaseBranch != "feature" || got.TargetSubdir != "src" || got.ResponseMode != "off" {
				t.Fatalf("lost submission or policy: %+v", got)
			}
			if err := s.Workflows.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Workflows.Runs()
			if err != nil || len(runs) != 1 || runs[0].RequestID != id {
				t.Fatal(runs, err)
			}
			if err := s.Workflows.Complete(got.Workflow.EventID, workflow.Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/1"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Workflows.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			runs, _ = s.Workflows.Runs()
			if len(runs) != 0 {
				t.Fatal(runs)
			}
		})
	}
}

func TestInputWorkflowRejectsClientSuppliedPolicyHash(t *testing.T) {
	t.Parallel()
	s, w := inputWorkflowServer(t, "prompt")
	c := config.Config{RepoURL: "https://github.com/acme/repo", Prompt: "Implement", Workflow: &config.WorkflowRunConfig{ID: w.ID, EventID: "made-up", PolicyHash: w.ExecutionPolicyHash()}}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.submitInputWorkflow(t.Context(), body, "prompt", func(context.Context, []byte) (string, error) {
		t.Fatal("accepted forged workflow policy")
		return "", nil
	})
	if err == nil {
		t.Fatal("expected error for a client-supplied workflow policy fingerprint")
	}
}

func TestInputWorkflowFallbackRetainsOriginalSubmission(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"paused", "feature off", "other repository", "other source", "no builder"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := inputWorkflowServer(t, "prompt")
			source := "prompt"
			switch mode {
			case "paused":
				d, _ := s.Workflows.Store.Load()
				d.Workflows[0].Enabled = false
				if _, err := s.Workflows.Store.Save(d); err != nil {
					t.Fatal(err)
				}
			case "feature off":
				s.Workflows.Enabled = func() bool { return false }
			case "other repository":
				d, _ := s.Workflows.Store.Load()
				d.Workflows[0].Repository = "acme/other"
				if _, err := s.Workflows.Store.Save(d); err != nil {
					t.Fatal(err)
				}
			case "other source":
				source = "json"
			case "no builder":
				s.Workflows = nil
			}
			body := []byte(`{"repoUrl":"https://github.com/acme/repo","prompt":"Original"}`)
			calls := 0
			_, err := s.submitInputWorkflow(t.Context(), body, source, func(_ context.Context, data []byte) (string, error) {
				calls++
				if string(data) != string(body) {
					t.Fatal("changed default submission")
				}
				return "original", nil
			})
			if err != nil || calls != 1 {
				t.Fatal(calls, err)
			}
		})
	}
}

func TestInputWorkflowRejectsConflictsAndDoesNotRetainSubmissionFailure(t *testing.T) {
	t.Parallel()
	s, w := inputWorkflowServer(t, "prompt")
	body := []byte(`{"repoUrl":"https://github.com/acme/repo","prompt":"Original"}`)
	_, err := s.submitInputWorkflow(t.Context(), body, "prompt", func(context.Context, []byte) (string, error) { return "", errors.New("queue unavailable") })
	if err == nil {
		t.Fatal("swallowed error")
	}
	runs, _ := s.Workflows.Runs()
	if len(runs) != 0 {
		t.Fatal(runs)
	}
	doc, _ := s.Workflows.Store.Load()
	w.ID = "second"
	doc.Workflows = append(doc.Workflows, w)
	if _, err := s.Workflows.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	_, err = s.submitInputWorkflow(t.Context(), body, "prompt", func(context.Context, []byte) (string, error) {
		t.Fatal("ambiguous workflow dispatched")
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "multiple enabled") {
		t.Fatal(err)
	}
}

func TestPromptEndpointRoutesEachInputSource(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"prompt", "json", "chat"} {
		t.Run(source, func(t *testing.T) {
			s, _ := inputWorkflowServer(t, source)
			s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
				return ReviewSettingsState{ChatEnabled: true, JSONEnabled: true, WorkflowEnabled: true}, nil
			}
			calls := 0
			s.SubmitLocalPrompt = func(_ context.Context, body []byte) (string, error) {
				var c config.Config
				if err := json.Unmarshal(body, &c); err != nil {
					t.Fatal(err)
				}
				if c.Workflow == nil || c.Workflow.ID != "input" {
					t.Fatal("source did not select workflow")
				}
				s.Broker.RecordTaskRunConfig("request-one", body)
				calls++
				return "request-one", nil
			}
			req := httptest.NewRequest(http.MethodPost, "/api/local-prompt", strings.NewReader(`{"repoUrl":"https://github.com/acme/repo","prompt":"Fix endpoint"}`))
			req.Header.Set("X-Agent-Task-Source", source)
			result := httptest.NewRecorder()
			s.handleLocalPrompt(result, req)
			if result.Code != http.StatusAccepted || calls != 1 {
				t.Fatal(result.Code, result.Body.String(), calls)
			}
			task, ok := s.Broker.Task("request-one")
			if !ok || !task.WorkflowTriggered || task.Source != source {
				t.Fatalf("workflow task lost its origin or input source: %+v", task)
			}
		})
	}
}

func TestJSONSubmissionCannotSetWorkflowMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"event ID only", `{"repoUrl":"https://github.com/acme/repo","prompt":"Fix endpoint","workflow":{"id":"input","eventId":"client-event"}}`, http.StatusAccepted},
		{"encoded policy hash", `"{\"repoUrl\":\"https://github.com/acme/repo\",\"prompt\":\"Fix endpoint\",\"workflow\":{\"id\":\"input\",\"eventId\":\"client-event\",\"policyHash\":\"client-policy\"}}"`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("", NewBroker())
			s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
				return ReviewSettingsState{JSONEnabled: true}, nil
			}
			var submitted bool
			s.SubmitLocalPrompt = func(_ context.Context, body []byte) (string, error) {
				run, err := localruntime.ParseRunConfigJSON(body)
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				submitted = true
				s.Broker.RecordTaskRunConfig("request-one", data)
				return "request-one", nil
			}
			req := httptest.NewRequest(http.MethodPost, "/api/local-prompt", strings.NewReader(tc.body))
			req.Header.Set(taskSourceHeader, "json")
			result := httptest.NewRecorder()
			s.handleLocalPrompt(result, req)
			if result.Code != tc.wantStatus || submitted != (tc.wantStatus == http.StatusAccepted) {
				t.Fatalf("status = %d, submitted = %t, response = %s", result.Code, submitted, result.Body)
			}
			if task, ok := s.Broker.Task("request-one"); submitted && (!ok || task.WorkflowTriggered || task.Source != "json") {
				t.Fatalf("expected a direct JSON task, got %+v", task)
			}
		})
	}
}
