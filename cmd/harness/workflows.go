package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func workflowRepositoryGate(path string, c config.Config) error {
	settings, err := localruntime.LoadRuntimeConfigUISettings(path)
	if err != nil {
		return err
	}
	for _, repo := range c.RepoList() {
		for _, disabled := range settings.DisabledRepositories {
			if workflow.CanonicalRepository(repo) == workflow.CanonicalRepository(disabled) {
				return fmt.Errorf("repository is disabled in Settings")
			}
		}
	}
	return nil
}
func workflowRunConfig(r workflow.Run) (config.Config, error) {
	w, i := r.Workflow, r.Item
	assign := w.Task.AssignCreator
	c := config.Config{RepoURL: "https://github.com/" + w.Repository + ".git", Prompt: w.Task.Instructions + "\n\nGitHub item: " + i.URL + "\nTitle: " + i.Title + "\n\n" + i.Body, PRTitle: i.Title, Draft: w.Task.Draft, AssignCreator: &assign, Workflow: &config.WorkflowRunConfig{Instructions: w.Task.Instructions, ID: w.ID, EventID: r.ID, Name: w.Name, ReviewCycles: w.ReviewCycles}}
	if i.Kind == "issue" {
		c.GitHubIssue = &config.GitHubIssueConfig{Repository: w.Repository, Number: i.Number, Title: i.Title, URL: i.URL, LinkMode: w.Task.IssueLinkMode}
	} else if w.Task.Kind == "review" {
		c.Review = &config.ReviewConfig{PRNumber: i.Number, PRURL: i.URL, HeadBranch: i.HeadBranch, Trigger: "manual", Writeback: "comment"}
	} else {
		c.BaseBranch = i.HeadBranch
		c.RequiresNonDefaultBranch = true
		c.LibraryTaskName = "resolve-pr-comments"
		c.Prompt += "\n\nRead current checks and review feedback. Repair this existing pull request on its head branch; do not open another pull request."
	}
	c.ApplyDefaults()
	return c, c.Validate()
}
func configureWorkflows(ctx context.Context, cfg localruntime.InitConfig, runner execx.Runner, server *web.Server, broker *web.Broker, enqueue func(context.Context, config.Config, bool, string, bool) (string, error), logf func(string, ...any)) error {
	root := filepath.Dir(cfg.RuntimeConfigPath)
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if _, err := os.Stat(store.Path); os.IsNotExist(err) {
		if err = migrateWorkflowSettings(cfg, store); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := store.Load(); err != nil {
		return err
	}
	gh := workflow.GitHub{Runner: runner}
	engine := &workflow.Engine{Store: store, StatePath: filepath.Join(root, "workflow-state.json"), Fetch: gh.Fetch, InspectPR: gh.Inspect, Merge: gh.Merge, Logf: logf}
	engine.Enabled = func() bool {
		s, err := localruntime.LoadRuntimeConfigUISettings(cfg.RuntimeConfigPath)
		return err == nil && s.WorkflowEnabled
	}
	engine.Allowed = func(repo string) bool {
		return workflowRepositoryGate(cfg.RuntimeConfigPath, config.Config{RepoURL: repo}) == nil
	}
	engine.Submit = func(ctx context.Context, r workflow.Run) (string, error) {
		if !engine.Enabled() || !engine.Allowed(r.Workflow.Repository) {
			return "", fmt.Errorf("workflow execution disabled")
		}
		c, err := workflowRunConfig(r)
		if err != nil {
			return "", err
		}
		return server.SubmitWorkflowRun(ctx, c, func(submitCtx context.Context, body []byte) (string, error) {
			parsed, err := localruntime.ParseRunConfigJSON(body)
			if err != nil {
				return "", err
			}
			return enqueue(submitCtx, parsed, false, "workflow", false)
		})
	}
	engine.Status = func(id string) workflow.Outcome {
		for _, t := range broker.TaskList() {
			if t.RequestID != id {
				continue
			}
			switch t.Status {
			case "completed", "no_changes", "merged":
				return workflow.Outcome{Status: "succeeded", PRURL: t.PRURL}
			case "error", "invalid", "stopped", "duplicate":
				return workflow.Outcome{Status: "failed", Reason: t.Error}
			}
			return workflow.Outcome{Status: "running"}
		}
		return workflow.Outcome{Status: "running", Reason: "Task history unavailable"}
	}
	engine.Reconcile = func(event string) string {
		for _, t := range broker.TaskList() {
			data, ok := broker.TaskRunConfig(t.RequestID)
			if !ok {
				continue
			}
			var c config.Config
			if json.Unmarshal(data, &c) == nil && c.Workflow != nil && c.Workflow.EventID == event {
				return t.RequestID
			}
		}
		return ""
	}
	server.Workflows = engine
	_ = ctx // The caller starts polling after server callbacks are configured.
	return nil
}
func migrateWorkflowSettings(cfg localruntime.InitConfig, store *workflow.Store) error {
	ui, err := localruntime.LoadRuntimeConfigUISettings(cfg.RuntimeConfigPath)
	if err != nil {
		return err
	}
	d := workflow.Document{Version: 1, Workflows: []workflow.Definition{}, Migration: "Legacy automation settings remain in config.json. Repository rules were copied as disabled drafts. Global review/merge preferences are no longer automatic; enable individual workflows explicitly."}
	// Preserve the exact legacy file before changing the source of automation behavior.
	if raw, err := os.ReadFile(cfg.RuntimeConfigPath); err == nil {
		backup := filepath.Join(filepath.Dir(store.Path), "workflow-legacy-settings.json")
		f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, err = f.Write(raw)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		} else if !os.IsExist(err) {
			return err
		}
	}
	for repo, settings := range ui.GitHubIssueRepositories {
		if !settings.AutoStart {
			continue
		}
		labels := settings.TriggerLabels
		if len(labels) == 0 {
			labels = []string{""}
		}
		for _, label := range labels {
			w := workflow.Definition{ID: fmt.Sprintf("migrated-%d", len(d.Workflows)+1), Name: "Migrated issue automation", Repository: repo, Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all", Rules: []workflow.Rule{}}, Task: workflow.Task{Kind: "implement", Instructions: "Implement the issue and run the relevant tests.", AssignCreator: ui.AssignCreator, IssueLinkMode: ui.GitHubIssueLinkMode}, ReviewCycles: cfg.ReviewWatch.ReviewCount(), Merge: workflow.Merge{Enabled: cfg.ReviewWatch.AutoMergeEnabled(), Method: cfg.ReviewWatch.MergeMethod, DeleteBranch: cfg.ReviewWatch.DeleteMergedBranchesEnabled()}}
			if w.Task.IssueLinkMode == "" {
				w.Task.IssueLinkMode = "reference"
			}
			if w.Merge.Method == "" {
				w.Merge.Method = "squash"
			}
			if label != "" {
				w.Trigger = workflow.Trigger{Event: "issue.labeled", Value: label}
				w.Name += " · " + label
			}
			if settings.OnlyAssigned {
				w.Filters.Rules = append(w.Filters.Rules, workflow.Rule{Field: "assignee", Operator: "is", Value: "@me"})
			}
			if strings.TrimSpace(settings.ExclusionLabel) != "" {
				w.Filters.Rules = append(w.Filters.Rules, workflow.Rule{Field: "label", Operator: "isNot", Value: settings.ExclusionLabel})
			}
			d.Workflows = append(d.Workflows, w)
		}
	}
	return store.Initialize(d)
}
