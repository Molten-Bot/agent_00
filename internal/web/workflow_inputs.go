package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func (s Server) submitInputWorkflow(ctx context.Context, body []byte, source string, submit func(context.Context, []byte) (string, error)) (string, error) {
	var header struct {
		Workflow *config.WorkflowRunConfig `json:"workflow"`
	}
	_ = json.Unmarshal(body, &header)
	c, err := localruntime.ParseRunConfigJSON(body)
	// Check both the resolved config form and the normalized input, which also accepts a JSON string.
	if (header.Workflow != nil && header.Workflow.PolicyHash != "") ||
		(err == nil && c.Workflow != nil && c.Workflow.PolicyHash != "") {
		return "", fmt.Errorf("workflow policy fingerprints are assigned by the runtime")
	}
	fallback := func() (string, error) {
		if err != nil {
			return submit(ctx, body) // Preserve the submitter's validation response.
		}
		return s.submitParsedWithIssueClaim(ctx, body, c, submit)
	}
	if header.Workflow != nil || s.Workflows == nil || !workflow.IsInputEvent("app."+source) || !s.Workflows.Enabled() {
		return fallback()
	}
	if err != nil {
		return "", err
	}
	var selected *workflow.Definition
	for _, repo := range c.Repos {
		w, err := s.Workflows.InputWorkflow(repo, source)
		if err != nil {
			return "", err
		}
		if w != nil {
			selected = w
		}
	}
	if selected == nil {
		return fallback()
	}
	if len(c.Repos) != 1 {
		return "", fmt.Errorf("a custom input workflow requires a single-repository submission; submit repositories separately")
	}
	if c.Review != nil || c.GitHubIssue != nil {
		return "", fmt.Errorf("custom app workflows accept repository tasks; use the GitHub workflow for an issue or pull request")
	}
	w := *selected
	if text := strings.TrimSpace(w.Task.Instructions); text != "" {
		c.Prompt += "\n\nWorkflow instructions:\n" + text
	}
	if w.Task.BaseBranch != "" {
		c.BaseBranch = w.Task.BaseBranch
	}
	if w.Task.TargetSubdir != "" {
		c.TargetSubdir = w.Task.TargetSubdir
	}
	if w.Task.Reviewers != nil {
		c.Reviewers = append([]string{}, (*w.Task.Reviewers)...)
	}
	c.AssignCreator = &w.Task.AssignCreator
	c.Complexity = w.Task.Complexity
	c.Draft = w.Task.Draft
	if err := c.Validate(); err != nil {
		return "", err
	}
	return s.Workflows.DispatchInput(ctx, w, func(run workflow.Run) (string, error) {
		c.Workflow = &config.WorkflowRunConfig{ID: w.ID, Name: w.Name, EventID: run.ID, Instructions: c.Prompt, ReviewCycles: w.ReviewCycles, ReviewAgent: w.ReviewAgent, PolicyHash: w.ExecutionPolicyHash()}
		data, err := json.Marshal(c)
		if err != nil {
			return "", err
		}
		return submit(ctx, data)
	})
}
