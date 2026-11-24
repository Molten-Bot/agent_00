package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestConfiguredWorkflowMigrationPreservesPoliciesAndDoesNotRecreateDeletedWorkflows(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := localruntime.InitConfig{Version: "v1", RuntimeConfigPath: filepath.Join(root, "config.json")}
	cfg.ApplyDefaults()
	yes := true
	cfg.ReviewWatch.ReviewLevel = "medium"
	cfg.ReviewWatch.AutoMerge = &yes
	cfg.ReviewWatch.DeleteMergedBranches = &yes
	cfg.ReviewWatch.MergeMethod = "rebase"
	original := []byte(`{"github_issues_enabled":true,"assign_creator":false,"github_issue_link_mode":"close","github_issue_repositories":{"acme/repo":{"auto_start":true,"only_assigned":true,"trigger_labels":["ready"],"exclusion_label":"blocked"}}}`)
	if err := os.WriteFile(cfg.RuntimeConfigPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := migrateWorkflowSettings(cfg, store); err != nil {
		t.Fatal(err)
	}
	repos := []web.GitHubRepo{{FullName: "acme/repo"}}
	if err := migrateConfiguredWorkflows(cfg, store, repos); err != nil {
		t.Fatal(err)
	}
	doc, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !doc.ConfigurationMigrated || !doc.LibrarySchedulesMigrated {
		t.Fatal(doc)
	}
	active := 0
	for _, w := range doc.Workflows {
		if !w.Enabled {
			continue
		}
		active++
		if w.ReviewCycles != 5 || !w.Merge.Enabled || !w.Merge.DeleteBranch || w.Merge.Method != "rebase" || w.Task.AssignCreator || w.Task.IssueLinkMode != "close" {
			t.Fatal(w)
		}
		if w.Trigger.Event == "issue.labeled" && (w.Trigger.Value != "ready" || len(w.Filters.Rules) != 2) {
			t.Fatal(w)
		}
	}
	if active != 4 {
		t.Fatalf("active=%d want prompt, JSON, chat, and issue workflows", active)
	}
	ui, err := localruntime.LoadRuntimeConfigUISettings(cfg.RuntimeConfigPath)
	if err != nil || !ui.WorkflowEnabled {
		t.Fatal(ui, err)
	}
	backup, _ := os.ReadFile(filepath.Join(root, "workflow-legacy-settings.json"))
	if string(backup) != string(original) {
		t.Fatal("original configuration not backed up")
	}
	doc.Workflows = nil
	if _, err := store.Save(doc); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfiguredWorkflows(cfg, store, repos); err != nil {
		t.Fatal(err)
	}
	doc, _ = store.Load()
	if len(doc.Workflows) != 0 {
		t.Fatal("deleted workflows recreated")
	}
	engine := &workflow.Engine{Store: store, Enabled: func() bool { return true }}
	engine.Enabled = func() bool { return false }
	if !engine.Owns("acme/repo", "issue.*") {
		t.Fatal("deletion reactivated legacy issue automation")
	}
}

func TestConfiguredWorkflowMigrationHandsOffLibraryScheduler(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := localruntime.InitConfig{Version: "v1", RuntimeConfigPath: filepath.Join(root, "config.json")}
	cfg.ApplyDefaults()
	legacy := &localruntime.LibrarySchedules{Path: cfg.RuntimeConfigPath, Init: cfg}
	saved, err := legacy.Save(localruntime.LibraryScheduleInput{LibraryTaskName: "unit-test-coverage", Repo: "acme/repo", IntervalHours: 12, BaseBranch: "feature", PromptContext: "Cover API", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := migrateWorkflowSettings(cfg, store); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfiguredWorkflows(cfg, store, nil); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load()
	found := false
	for _, w := range doc.Workflows {
		if w.ID == importedScheduleID(saved) {
			found = true
			if !w.Enabled || !w.Trigger.StartAt.Equal(saved.NextRunAt) || w.Task.BaseBranch != "feature" || !w.Task.Draft {
				t.Fatal(w)
			}
		}
	}
	if !found {
		t.Fatal("schedule missing")
	}
	engine := &workflow.Engine{Store: store, Enabled: func() bool { return true }}
	if !workflowOwnsLibrarySchedule(engine, saved) {
		t.Fatal("legacy scheduler still owns imported schedule")
	}
	doc.Workflows = nil
	store.Save(doc)
	if !workflowOwnsLibrarySchedule(engine, saved) {
		t.Fatal("deleting workflow reactivated old schedule")
	}
}

func TestConfiguredWorkflowMigrationPreservesCustomInputOverrides(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := localruntime.InitConfig{Version: "v1", RuntimeConfigPath: filepath.Join(root, "config.json")}
	cfg.ApplyDefaults()
	if err := os.WriteFile(cfg.RuntimeConfigPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	custom := workflow.Definition{ID: "custom", Name: "My prompt workflow", Repository: "acme/repo", Enabled: true, Trigger: workflow.Trigger{Event: "app.prompt"}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", Instructions: "Keep these instructions", IssueLinkMode: "reference"}, ReviewCycles: 2, Merge: workflow.Merge{Method: "squash"}}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{custom}}); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfiguredWorkflows(cfg, store, []web.GitHubRepo{{FullName: "acme/repo"}}); err != nil {
		t.Fatal(err)
	}
	engine := &workflow.Engine{Store: store, Enabled: func() bool { return true }}
	got, err := engine.InputWorkflow("acme/repo", "prompt")
	if err != nil || got == nil || got.ID != "custom" || got.Task.Instructions != custom.Task.Instructions || got.ReviewCycles != 2 {
		t.Fatal(got, err)
	}
	doc, _ := store.Load()
	doc.ConfigurationMigrated = false
	doc.MigratedRepositories = nil
	doc.LibrarySchedulesMigrated = false
	if _, err := store.Save(doc); err != nil {
		t.Fatal(err)
	}
	doc, _ = store.Load()
	if !doc.ConfigurationMigrated || !doc.LibrarySchedulesMigrated || len(doc.MigratedRepositories) != 1 {
		t.Fatal("older editor erased migration receipts", doc)
	}
}

func TestConfiguredWorkflowMigrationSupportsLargeRepositoryCatalog(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := localruntime.InitConfig{Version: "v1", RuntimeConfigPath: filepath.Join(root, "config.json")}
	cfg.ApplyDefaults()
	if err := os.WriteFile(cfg.RuntimeConfigPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	repos := make([]web.GitHubRepo, 200)
	for i := range repos {
		repos[i].FullName = fmt.Sprintf("acme/repo-%d", i)
	}
	if err := migrateConfiguredWorkflows(cfg, store, repos); err != nil {
		t.Fatal(err)
	}
	doc, err := store.Load()
	if err != nil || len(doc.Workflows) != 600 {
		t.Fatal(len(doc.Workflows), err)
	}
}
