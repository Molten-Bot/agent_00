package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Molten-Bot/agent_00/internal/config"
	"io"
	"net/http"

	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func workflowError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
func (s Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if s.Workflows == nil {
		workflowError(w, 503, errors.New("workflow store unavailable"))
		return
	}
	if r.Method == http.MethodGet {
		d, err := s.Workflows.Store.Load()
		if err != nil {
			workflowError(w, 500, err)
			return
		}
		writeJSON(w, 200, d)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		workflowError(w, 400, err)
		return
	}
	d, err := workflow.Decode(data)
	if err != nil {
		workflowError(w, 400, err)
		return
	}
	d, err = s.Workflows.Store.Save(d)
	if err != nil {
		status := 400
		if errors.Is(err, workflow.ErrConflict) {
			status = 409
		}
		workflowError(w, status, err)
		return
	}
	writeJSON(w, 200, d)
}
func (s Server) handleWorkflowPreview(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if s.Workflows == nil {
		workflowError(w, 503, errors.New("workflow store unavailable"))
		return
	}
	var def workflow.Definition
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		workflowError(w, 400, err)
		return
	}
	results, err := s.Workflows.Preview(r.Context(), def)
	if err != nil {
		workflowError(w, 400, err)
		return
	}
	writeJSON(w, 200, results)
}
func (s Server) handleWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.Workflows == nil {
		workflowError(w, 503, errors.New("workflow store unavailable"))
		return
	}
	runs, err := s.Workflows.Runs()
	if err != nil {
		workflowError(w, 500, err)
		return
	}
	writeJSON(w, 200, runs)
}

// SubmitWorkflowRun retains durable issue ownership, fresh comments, dependencies,
// and recovery used by manual issue tasks. PR workflows use the normal task queue.
func (s Server) SubmitWorkflowRun(ctx context.Context, run config.Config, submit func(context.Context, []byte) (string, error)) (string, error) {
	if run.GitHubIssue == nil {
		data, err := json.Marshal(run)
		if err != nil {
			return "", err
		}
		return submit(ctx, data)
	}
	settings, enabled, err := s.workflowIssueSettings(run)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", fmt.Errorf("workflow is disabled")
	}
	return s.claimAndSubmitIssue(ctx, run, settings, "", submit)
}
func (s Server) workflowIssueSettings(run config.Config) (ReviewSettingsState, bool, error) {
	if s.Workflows == nil || run.Workflow == nil || !s.Workflows.Enabled() {
		return ReviewSettingsState{}, false, nil
	}
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		return ReviewSettingsState{}, false, err
	}
	for _, w := range doc.Workflows {
		if w.ID == run.Workflow.ID && w.Enabled && s.Workflows.Allowed(w.Repository) && run.GitHubIssue != nil && workflow.CanonicalRepository(w.Repository) == workflow.CanonicalRepository(run.GitHubIssue.Repository) {
			return ReviewSettingsState{WorkflowEnabled: true, GitHubIssuesEnabled: true, GitHubIssueAutoStartAssigned: true, GitHubIssueAutoStartExclusionLabel: "agent_00-in-progress"}, true, nil
		}
	}
	return ReviewSettingsState{}, false, nil
}
