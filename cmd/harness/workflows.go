package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Molten-Bot/agent_00/internal/library"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	assign := w.Task.AssignCreator
	c.AssignCreator = &assign
	c.Draft = w.Task.Draft
	if w.Task.TargetSubdir != "" {
		c.TargetSubdir = w.Task.TargetSubdir
	}
	if w.Task.Reviewers != nil {
		c.Reviewers = append([]string(nil), (*w.Task.Reviewers)...)
	}
	c.Workflow = &config.WorkflowRunConfig{Instructions: resolvedPrompt, ID: w.ID, EventID: r.ID, Name: w.Name, ReviewCycles: w.ReviewCycles}
	if i.Kind == "issue" {
		c.PRTitle = i.Title
		c.GitHubIssue = &config.GitHubIssueConfig{Repository: w.Repository, Number: i.Number, Title: i.Title, URL: i.URL, LinkMode: w.Task.IssueLinkMode}
	} else if i.Kind == "pr" && w.Task.Kind == "review" {
		c.Review = &config.ReviewConfig{PRNumber: i.Number, PRURL: i.URL, HeadBranch: i.HeadBranch, Trigger: "manual", Writeback: "comment"}
	} else if i.Kind == "pr" {
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
	// Repository discovery supplies the scope for formerly global defaults.
	// Retry failed discovery/migration; a persisted receipt makes success one-time.
	var migrationMu sync.Mutex
	server.MigrateWorkflowDefaults = func(repos []web.GitHubRepo) error {
		migrationMu.Lock()
		defer migrationMu.Unlock()
		return migrateConfiguredWorkflows(cfg, store, repos)
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
			id, err := enqueue(submitCtx, parsed, false, "workflow", false)
			if err == nil && parsed.LibraryTaskName != "" {
				if usageErr := localruntime.IncrementRuntimeConfigLibraryTaskUsage(cfg.RuntimeConfigPath, cfg, parsed.LibraryTaskName); usageErr != nil {
					logf("workflow library usage status=warn err=%q", usageErr)
				}
			}
			return id, err
		})
	}
	engine.Active = func(w workflow.Definition) bool {
		doc, err := store.Load()
		if err != nil {
			return true
		}
		if !doc.ConfigurationMigrated && configuredScheduleOwnsWorkflow(cfg.RuntimeConfigPath, w) {
			return true
		}
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
	d := workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{}, Migration: "Configured automation remains active in config.json. Repository rules were copied as disabled drafts; enable a custom workflow to override its event family for that repository."}
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

func migrateLibrarySchedules(cfg localruntime.InitConfig, store *workflow.Store) error {
	doc, err := store.Load()
	if err != nil {
		return err
	}
	if doc.LibrarySchedulesMigrated {
		return nil
	}
	legacy := &localruntime.LibrarySchedules{Path: cfg.RuntimeConfigPath, Init: cfg}
	schedules, err := legacy.List()
	if err != nil {
		return err
	}
	ui, err := localruntime.LoadRuntimeConfigUISettings(cfg.RuntimeConfigPath)
	if err != nil {
		return err
	}
	for _, schedule := range schedules {
		sum := sha256.Sum256([]byte(schedule.Key))
		id := "library-" + hex.EncodeToString(sum[:12])
		found := false
		for _, w := range doc.Workflows {
			if w.ID == id {
				found = true
				break
			}
		}
		if found {
			continue
		}
		next := schedule.NextRunAt
		reviewers := append([]string{}, schedule.Reviewers...)
		targetSubdir := strings.TrimSpace(schedule.TargetSubdir)
		if targetSubdir == "" {
			targetSubdir = "."
		}
		name := "Scheduled " + schedule.LibraryTaskName
		if len(name) > 100 {
			name = name[:100]
		}
		doc.Workflows = append(doc.Workflows, workflow.Definition{ID: id, Name: name, Repository: strings.SplitN(schedule.Key, "@", 2)[1], Enabled: true, Trigger: workflow.Trigger{Event: "schedule", IntervalHours: float64(schedule.IntervalHours), StartAt: &next}, Filters: workflow.Filters{Mode: "all", Rules: []workflow.Rule{}}, Task: workflow.Task{Kind: "implement", LibraryTaskName: schedule.LibraryTaskName, Instructions: schedule.PromptContext, BaseBranch: schedule.BaseBranch, TargetSubdir: targetSubdir, Reviewers: &reviewers, Draft: schedule.Draft, AssignCreator: ui.AssignCreator, IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash", RequireApproval: true}})
	}
	doc.LibrarySchedulesMigrated = true
	if len(schedules) > 0 {
		doc.Migration += " Library schedules moved here with their next due time and task options. They run only while the Workflow feature is enabled. The legacy scheduler is stopped."
	}
	_, err = store.Save(doc)
	return err
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
