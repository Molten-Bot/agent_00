package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestWorkflowRunConfigPolicies(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"implement", "review", "repair"} {
		t.Run(kind, func(t *testing.T) {
			w := workflow.Definition{ID: "one", Name: "Workflow", Repository: "acme/repo", ReviewCycles: 3, Task: workflow.Task{Kind: kind, Instructions: "Do the work", IssueLinkMode: "close"}}
			i := workflow.Item{Number: 12, Kind: "issue", URL: "https://github.com/acme/repo/issues/12", Title: "A task", HeadBranch: "feature"}
			if kind != "implement" {
				i.Kind = "pr"
				i.URL = "https://github.com/acme/repo/pull/12"
			}
			c, err := workflowRunConfig(workflow.Run{ID: "event", Workflow: w, Item: i})
			if err != nil {
				t.Fatal(err)
			}
			if c.Workflow.ReviewCycles != 3 || c.Workflow.EventID != "event" || *c.AssignCreator {
				t.Fatal(c)
			}
			if kind == "review" && (c.Review == nil || c.Review.AutoMerge || c.Review.Writeback != "summary-comment") {
				t.Fatal("review must report outcome before merge", c)
			}
			if kind == "repair" && (c.BaseBranch != "feature" || !c.RequiresNonDefaultBranch) {
				t.Fatal("repair must reuse existing branch")
			}
		})
	}
}
func TestWorkflowRepositoryAdmission(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"disabled_repositories":["acme/blocked"]}`), 0600)
	for _, repo := range []string{"acme/blocked", "https://github.com/acme/blocked.git", "git@github.com:acme/blocked.git"} {
		if workflowRepositoryGate(path, config.Config{RepoURL: repo}) == nil {
			t.Fatalf("accepted %s", repo)
		}
	}
	if err := workflowRepositoryGate(path, config.Config{RepoURL: "acme/allowed"}); err != nil {
		t.Fatal(err)
	}
}
func TestWorkflowMigrationPreservesOriginalAndIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	original := []byte(`{"github_issue_repositories":{"acme/repo":{"auto_start":true,"only_assigned":true,"trigger_labels":["ready"],"exclusion_label":"in-progress"}}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := localruntime.InitConfig{RuntimeConfigPath: path}
	cfg.ApplyDefaults()
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := migrateWorkflowSettings(cfg, store); err != nil {
		t.Fatal(err)
	}
	d, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Workflows) != 1 || d.Workflows[0].Enabled || d.Workflows[0].IncludeBacklog {
		t.Fatal(d)
	}
	d.Workflows[0].Name = "Edited"
	if _, err = store.Save(d); err != nil {
		t.Fatal(err)
	}
	if err = migrateWorkflowSettings(cfg, store); err != nil {
		t.Fatal(err)
	}
	d, _ = store.Load()
	if d.Workflows[0].Name != "Edited" {
		t.Fatal("migration overwrote user edits")
	}
	backup, _ := os.ReadFile(filepath.Join(root, "workflow-legacy-settings.json"))
	if string(backup) != string(original) {
		t.Fatal("legacy settings not preserved")
	}
}
