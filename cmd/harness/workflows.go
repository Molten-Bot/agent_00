package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/library"
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

type workflowLibraryResolver func(string, string, string, string) (config.Config, error)

func workflowRunConfig(r workflow.Run) (config.Config, error) {
	return workflowRunConfigWithLibrary(r, func(name, repo, branch, context string) (config.Config, error) {
		catalog, err := library.LoadCustomizedCatalog(library.DefaultDir)
		if err != nil {
			return config.Config{}, err
		}
		return catalog.ExpandRunConfigWithContext(name, repo, branch, context)
	})
}
func workflowRunConfigWithLibrary(r workflow.Run, resolve workflowLibraryResolver) (config.Config, error) {
	w, i := r.Workflow, r.Item
	repo := "https://github.com/" + w.Repository + ".git"
	branch := w.Task.BaseBranch
	if i.Kind == "pr" {
		branch = i.HeadBranch
	}
	c := config.Config{RepoURL: repo, Prompt: w.Task.Instructions, BaseBranch: branch}
	if w.Task.LibraryTaskName != "" {
		var err error
		c, err = resolve(w.Task.LibraryTaskName, repo, branch, w.Task.Instructions)
		if err != nil {
			return config.Config{}, fmt.Errorf("resolve workflow library prompt: %w", err)
		}
	}
	resolvedPrompt := c.Prompt
	if i.Kind == "issue" || i.Kind == "pr" {
		c.Prompt += "\n\nGitHub item: " + i.URL + "\nTitle: " + i.Title + "\n\n" + i.Body
	}
	if i.Kind == "action" {
		c.Prompt += fmt.Sprintf("\n\nFailed GitHub Actions run: %s\nWorkflow: %s\nRun ID: %d\nAttempt: %d\nHead branch: %s\nHead commit: %s",
			i.URL, strings.TrimSpace(w.Trigger.Value), i.ActionRunID, i.ActionRunAttempt, i.HeadBranch, i.HeadSHA)
	}
	assign := w.Task.AssignCreator
	c.AssignCreator = &assign
	c.Complexity = w.Task.Complexity
	c.Draft = w.Task.Draft
	if w.Task.TargetSubdir != "" {
		c.TargetSubdir = w.Task.TargetSubdir
	}
	if w.Task.Reviewers != nil {
		c.Reviewers = append([]string(nil), (*w.Task.Reviewers)...)
	}
	c.Workflow = &config.WorkflowRunConfig{
		Instructions: resolvedPrompt, ID: w.ID, EventID: r.ID, Name: w.Name,
		ReviewCycles: w.ReviewCycles, ReviewAgent: w.ReviewAgent,
		PRAssignee: w.Task.PRAssignee, PRLabels: w.Task.PRLabels, PolicyHash: w.ExecutionPolicyHash(),
	}
	if i.Kind == "issue" {
		c.PRTitle = i.Title
		c.GitHubIssue = &config.GitHubIssueConfig{Repository: w.Repository, Number: i.Number, Title: i.Title, URL: i.URL, LinkMode: w.Task.IssueLinkMode}
	} else if i.Kind == "pr" && w.Task.Kind == "review" {
		c.Review = &config.ReviewConfig{PRNumber: i.Number, PRURL: i.URL, HeadBranch: i.HeadBranch, Trigger: "manual", Writeback: "comment"}
	} else if i.Kind == "pr" {
		c.Workflow.RepairPRURL = i.URL
		c.RequiresNonDefaultBranch = true
		if c.LibraryTaskName == "" {
			c.LibraryTaskName = "resolve-pr-comments"
		}
		c.Prompt += "\n\nRead current checks and review feedback. Repair this existing pull request on its head branch; do not open another pull request."
	}
	c.ApplyDefaults()
	return c, c.Validate()
}

// Stored workflow configurations already contain their resolved prompt. Parsing
// them as library selections would reject or re-expand that captured prompt.
func parseResolvedWorkflowConfig(body []byte) (config.Config, error) {
	var c config.Config
	if err := json.Unmarshal(body, &c); err != nil {
		return c, err
	}
	c.ApplyDefaults()
	return c, c.Validate()
}
func configureWorkflows(ctx context.Context, cfg localruntime.InitConfig, runner execx.Runner, server *web.Server, broker *web.Broker, enqueue func(context.Context, config.Config, string, bool) (string, error), logf func(string, ...any)) error {
	root := filepath.Dir(cfg.RuntimeConfigPath)
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	// Workflow creation is explicit. Upgrading must never derive definitions
	// from legacy automation settings or library schedules.
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{}}); err != nil {
		return err
	}
	gh := workflow.GitHub{Runner: runner, Cooldown: server.GitHubCooldown, Logf: logf}
	reviewer := app.New(runner)
	engine := &workflow.Engine{Store: store, StatePath: filepath.Join(root, "workflow-state.json"), Fetch: gh.Fetch, InspectPR: gh.Inspect, FinishReview: reviewer.ResumeReviewGate, Merge: gh.Merge, Logf: logf}
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
		c, err := workflowRunConfigWithLibrary(r, func(name, repo, branch, promptContext string) (config.Config, error) {
			var catalog library.Catalog
			var err error
			settings, err := localruntime.LoadRuntimeConfigUISettings(cfg.RuntimeConfigPath)
			if err != nil {
				return config.Config{}, err
			}
			if settings.DefaultLibraryEnabled {
				catalog, err = library.LoadCustomizedCatalog(library.DefaultDir)
			} else {
				catalog, err = library.LoadCustomLibraryCatalog()
			}
			if err != nil {
				return config.Config{}, err
			}
			return catalog.ExpandRunConfigWithContext(name, repo, branch, promptContext)
		})
		if err != nil {
			return "", err
		}
		return server.SubmitWorkflowRun(ctx, c, func(submitCtx context.Context, body []byte) (string, error) {
			parsed, err := parseResolvedWorkflowConfig(body)
			if err != nil {
				return "", err
			}
			id, err := enqueue(submitCtx, parsed, "workflow", false)
			if err == nil && parsed.LibraryTaskName != "" {
				if usageErr := localruntime.IncrementRuntimeConfigLibraryTaskUsage(cfg.RuntimeConfigPath, cfg, parsed.LibraryTaskName); usageErr != nil {
					logf("workflow library usage status=warn err=%q", usageErr)
				}
			}
			return id, err
		})
	}
	engine.Active = func(w workflow.Definition) bool {
		for _, t := range broker.TaskList() {
			switch t.Status {
			case "completed", "no_changes", "merged", "error", "invalid", "stopped", "duplicate":
				continue
			}
			repos := append([]string{t.Repo}, t.Repos...)
			for _, repo := range repos {
				if workflow.CanonicalRepository(repo) == workflow.CanonicalRepository(w.Repository) {
					return true
				}
			}
		}
		return false
	}
	engine.Status = func(id string) workflow.Outcome {
		// The review outcome survives workspace cleanup and a daemon restart
		// between task completion and the workflow journal update.
		archived, archiveErr := workflow.LoadReviewOutcome(root, id)
		for _, t := range broker.TaskList() {
			if t.RequestID != id {
				continue
			}
			switch t.Status {
			case "completed", "no_changes", "merged":
				if archiveErr == nil {
					return archived
				}
				return workflow.Outcome{Status: "succeeded", PRURL: t.PRURL}
			case "error", "invalid", "stopped", "duplicate":
				if archiveErr == nil {
					if archived.Status == workflow.OutcomeReviewPending {
						return archived
					}
					return workflow.Outcome{Status: "failed", PRURL: archived.PRURL, Reason: t.Error, ReviewInterrupted: archived.ReviewInterrupted}
				}
				return workflow.Outcome{Status: "failed", Reason: t.Error}
			}
			return workflow.Outcome{Status: "running"}
		}
		if archiveErr == nil {
			return archived
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
func parseWorkflowOrSelection(body []byte) (config.Config, error) {
	var header struct {
		Workflow *config.WorkflowRunConfig `json:"workflow"`
		Prompt   string                    `json:"prompt"`
	}
	if json.Unmarshal(body, &header) == nil && header.Workflow != nil && header.Workflow.EventID != "" && header.Prompt != "" {
		return parseResolvedWorkflowConfig(body)
	}
	return localruntime.ParseRunConfigJSON(body)
}

func retryWorkflowTask(ctx context.Context, engine *workflow.Engine, task web.Task, cfg config.Config, submit func(config.Config) (string, error)) (string, error) {
	if cfg.Workflow == nil || cfg.Workflow.PolicyHash == "" {
		return submit(cfg)
	}
	switch task.Status {
	case "error", "invalid", "stopped", "duplicate":
	default:
		return submit(cfg)
	}
	if cfg.GitHubIssue == nil && (cfg.Workflow.ResumePRURL != "" || app.IsWorkflowPRMetadataFailure(task.Error)) {
		target := cfg.Workflow.ResumePRURL
		if target == "" {
			target = task.PRURL
		}
		if target == "" {
			return "", fmt.Errorf("the original pull request is required to retry workflow PR metadata")
		}
		policy, err := engine.PRMetadataRetryPolicy(*cfg.Workflow)
		if err != nil {
			return "", err
		}
		policy.ResumePRURL = target
		cfg.Workflow = &policy
	}
	item := workflow.Item{Repository: workflow.CanonicalRepository(cfg.RepoURL)}
	if cfg.GitHubIssue != nil {
		item.Number = cfg.GitHubIssue.Number
	} else if cfg.Review != nil {
		item.Number, item.URL = cfg.Review.PRNumber, cfg.Review.PRURL
	} else if _, number, ok := strings.Cut(strings.TrimRight(task.PRURL, "/"), "/pull/"); ok {
		item.Number, _ = strconv.Atoi(number)
		item.URL = task.PRURL
	}
	return engine.DispatchRetry(ctx, cfg.Workflow.ID, cfg.Workflow.PolicyHash, cfg.Workflow.EventID, item, func(run workflow.Run) (string, error) {
		if run.ID != "" {
			policy := *cfg.Workflow
			policy.EventID = run.ID
			cfg.Workflow = &policy
		}
		return submit(cfg)
	})
}
