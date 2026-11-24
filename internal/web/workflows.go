package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Molten-Bot/agent_00/internal/config"
	"io"
	"net/http"
	"strings"

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
	if err = s.validateWorkflowLibrary(d); err != nil {
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
	if err := dec.Decode(new(any)); err != io.EOF {
		workflowError(w, 400, errors.New("expected one JSON document"))
		return
	}
	results, err := s.Workflows.Preview(r.Context(), def)
	if err != nil {
		workflowError(w, 400, err)
		return
	}
	writeJSON(w, 200, results)
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

func (s Server) handleWorkflowSchedules(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.Workflows == nil {
		workflowError(w, 503, errors.New("workflow store unavailable"))
		return
	}
	status, err := s.Workflows.ScheduleStatus()
	if err != nil {
		workflowError(w, 500, err)
		return
	}
	writeJSON(w, 200, status)
}

func (s Server) validateWorkflowLibrary(doc workflow.Document) error {
	needed := false
	for _, w := range doc.Workflows {
		if w.Enabled && w.Task.LibraryTaskName != "" {
			needed = true
		}
	}
	if !needed {
		return nil
	}
	if s.LoadLibraryTasks == nil {
		return fmt.Errorf("library catalog unavailable")
	}
	tasks, err := s.LoadLibraryTasks()
	if err != nil {
		return err
	}
	for _, w := range doc.Workflows {
		if !w.Enabled || w.Task.LibraryTaskName == "" {
			continue
		}
		found := false
		for _, task := range tasks {
			if task.Name != w.Task.LibraryTaskName {
				continue
			}
			found = true
			if task.PromptContextRequired && strings.TrimSpace(w.Task.Instructions) == "" {
				return fmt.Errorf("workflow %q: library prompt requires additional context", w.Name)
			}
			if task.RequiresNonDefaultBranch && !strings.HasPrefix(w.Trigger.Event, "pr.") && strings.TrimSpace(w.Task.BaseBranch) != "" {
				guard := config.Config{RepoURL: "https://github.com/" + w.Repository + ".git", Prompt: "Validate library branch", LibraryTaskName: task.Name, RequiresNonDefaultBranch: true, BaseBranch: w.Task.BaseBranch}
				guard.ApplyDefaults()
				if err := guard.Validate(); err != nil {
					return fmt.Errorf("workflow %q: %w", w.Name, err)
				}
			}
			if task.RequiresNonDefaultBranch && !strings.HasPrefix(w.Trigger.Event, "pr.") && strings.TrimSpace(w.Task.BaseBranch) == "" {
				return fmt.Errorf("workflow %q: library task requires a non-default base branch", w.Name)
			}
		}
		if !found {
			return fmt.Errorf("workflow %q: library task %q is unavailable", w.Name, w.Task.LibraryTaskName)
		}
	}
	return nil
}
