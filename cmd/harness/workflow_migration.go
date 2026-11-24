package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

// migrateConfiguredWorkflows transfers the existing installation's defaults once
// its repositories are known. Later starts must preserve edits and deletions.
func migrateConfiguredWorkflows(cfg localruntime.InitConfig, store *workflow.Store, repos []web.GitHubRepo) error {
	doc, err := store.Load()
	if err != nil || doc.ConfigurationMigrated {
		return err
	}
	ui, err := localruntime.LoadRuntimeConfigUISettings(cfg.RuntimeConfigPath)
	if err != nil {
		return err
	}
	if err := migrateLibrarySchedules(cfg, store); err != nil {
		return err
	}
	doc, err = store.Load()
	if err != nil {
		return err
	}
	// Unedited imported schedules were still executed by the legacy scheduler,
	// which applied the installation's review and merge policy.
	schedules, err := (&localruntime.LibrarySchedules{Path: cfg.RuntimeConfigPath, Init: cfg}).List()
	if err != nil {
		return err
	}
	for i := range doc.Workflows {
		for _, schedule := range schedules {
			if unchangedImportedSchedule(doc.Workflows[i], schedule) {
				doc.Workflows[i].ReviewCycles = cfg.ReviewWatch.ReviewCount()
				doc.Workflows[i].Merge = workflow.Merge{Enabled: cfg.ReviewWatch.AutoMergeEnabled(), Method: cfg.ReviewWatch.MergeMethod, DeleteBranch: cfg.ReviewWatch.DeleteMergedBranchesEnabled()}
				if doc.Workflows[i].Merge.Method == "" {
					doc.Workflows[i].Merge.Method = "squash"
				}
			}
		}
	}
	names := map[string]bool{}
	for _, repo := range repos {
		names[workflow.CanonicalRepository(repo.FullName)] = true
	}
	for repo := range ui.GitHubIssueRepositories {
		names[workflow.CanonicalRepository(repo)] = true
	}
	for _, w := range doc.Workflows {
		names[w.Repository] = true
	}
	delete(names, "")
	for repo := range names {
		doc.MigratedRepositories = append(doc.MigratedRepositories, repo)
	}
	sort.Strings(doc.MigratedRepositories)
	originalWorkflows := append([]workflow.Definition(nil), doc.Workflows...)
	appendDefault := func(w workflow.Definition) {
		for _, existing := range originalWorkflows {
			if existing.Enabled && existing.Repository == w.Repository && strings.HasPrefix(existing.Trigger.Event, "issue.") && strings.HasPrefix(w.Trigger.Event, "issue.") {
				return
			}
		}
		for _, existing := range doc.Workflows {
			if existing.Enabled && existing.Repository == w.Repository && existing.Trigger.Event == w.Trigger.Event && existing.Trigger.Value == w.Trigger.Value {
				return
			}
		}
		for n := len(doc.Workflows) + 1; ; n++ {
			w.ID = fmt.Sprintf("default-%d", n)
			found := false
			for _, existing := range doc.Workflows {
				if existing.ID == w.ID {
					found = true
					break
				}
			}
			if !found {
				break
			}
		}
		doc.Workflows = append(doc.Workflows, w)
	}
	for _, repo := range doc.MigratedRepositories {
		base := workflow.Definition{Repository: repo, Enabled: true, Filters: workflow.Filters{Mode: "all", Rules: []workflow.Rule{}}, Task: workflow.Task{Kind: "implement", AssignCreator: ui.AssignCreator, IssueLinkMode: ui.GitHubIssueLinkMode}, ReviewCycles: cfg.ReviewWatch.ReviewCount(), Merge: workflow.Merge{Enabled: cfg.ReviewWatch.AutoMergeEnabled(), Method: cfg.ReviewWatch.MergeMethod, DeleteBranch: cfg.ReviewWatch.DeleteMergedBranchesEnabled()}}
		if base.Task.IssueLinkMode == "" {
			base.Task.IssueLinkMode = "reference"
		}
		if base.Merge.Method == "" {
			base.Merge.Method = "squash"
		}
		for _, source := range []string{"prompt", "json", "chat"} {
			w := base
			w.Name = "Default " + source + " workflow"
			w.Trigger.Event = "app." + source
			appendDefault(w)
		}
		settings, exists := ui.GitHubIssueRepositories[repo]
		if ui.GitHubIssueRepositories == nil {
			settings = localruntime.GitHubIssueRepositorySettings{AutoStart: ui.GitHubIssueAutoStartAssigned, OnlyAssigned: ui.GitHubIssueAutoStartOnlyAssigned, TriggerLabels: ui.GitHubIssueAutoStartLabels, ExclusionLabel: ui.GitHubIssueAutoStartExclusionLabel}
			exists = true
		}
		if !ui.GitHubIssuesEnabled || !exists || !settings.AutoStart {
			continue
		}
		labels := settings.TriggerLabels
		if len(labels) == 0 {
			labels = []string{""}
		}
		for _, label := range labels {
			w := base
			w.Name = "Default issue workflow"
			w.Task.Instructions = "Implement the issue and run the relevant tests."
			w.Trigger = workflow.Trigger{Event: "issue.opened"}
			if label != "" {
				w.Trigger = workflow.Trigger{Event: "issue.labeled", Value: label}
			}
			w.Filters.Rules = []workflow.Rule{}
			if settings.OnlyAssigned {
				w.Filters.Rules = append(w.Filters.Rules, workflow.Rule{Field: "assignee", Operator: "is", Value: "@me"})
			}
			if strings.TrimSpace(settings.ExclusionLabel) != "" {
				w.Filters.Rules = append(w.Filters.Rules, workflow.Rule{Field: "label", Operator: "isNot", Value: settings.ExclusionLabel})
			}
			appendDefault(w)
		}
	}
	doc.ConfigurationMigrated = true
	doc.Migration = "Existing configuration was imported automatically. Manage task policies and automation in Workflow."
	if err := doc.Validate(); err != nil {
		return err
	}
	// Enable the new owner before recording completion. A retry cannot duplicate
	// imports, and a later deliberate feature toggle remains untouched.
	enabled := true
	if err := localruntime.SaveRuntimeConfigReviewDefaultLibraryAndGitHubIssuesSettings(cfg.RuntimeConfigPath, cfg, cfg.ReviewWatch, nil, nil, nil, nil, nil, nil, nil, localruntime.RuntimeConfigUISettingsUpdate{WorkflowEnabled: &enabled}); err != nil {
		return err
	}
	_, err = store.Save(doc)
	return err
}
