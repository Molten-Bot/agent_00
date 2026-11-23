package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestWorkflowLibraryPromptResolvesPerOccurrenceAndSurvivesSubmission(t *testing.T) {
	t.Parallel()
	revision := 0
	reviewers := []string{"reviewer"}
	resolver := func(name, repo, branch, context string) (config.Config, error) {
		revision++
		return config.Config{RepoURL: repo, LibraryTaskName: name, LibraryTaskDisplayName: "Library task", Prompt: fmt.Sprintf("Library revision %d: %s", revision, context), BaseBranch: branch, CommitMessage: "test: library", TargetSubdir: "library-dir", RequiresNonDefaultBranch: true}, nil
	}
	r := workflow.Run{ID: "occurrence-1", Workflow: workflow.Definition{ID: "scheduled", Name: "Scheduled coverage", Repository: "acme/repo", Task: workflow.Task{Kind: "implement", LibraryTaskName: "unit-test-coverage", Instructions: "Focus on regressions", BaseBranch: "feature", TargetSubdir: "src", Reviewers: &reviewers, Draft: true}}, Item: workflow.Item{Kind: "schedule"}}
	first, err := workflowRunConfigWithLibrary(r, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Prompt, "Library revision 1") || strings.Contains(first.Prompt, "GitHub item") || first.TargetSubdir != "src" || !first.Draft || !first.RequiresNonDefaultBranch || first.Workflow.Instructions != first.Prompt {
		t.Fatal(first)
	}
	r.ID = "occurrence-2"
	second, err := workflowRunConfigWithLibrary(r, resolver)
	if err != nil || !strings.Contains(second.Prompt, "Library revision 2") {
		t.Fatal(second, err)
	}
	data, _ := json.Marshal(first)
	parsed, err := parseWorkflowOrSelection(data)
	if err != nil || parsed.Prompt != first.Prompt || parsed.LibraryTaskName != first.LibraryTaskName {
		t.Fatal(parsed, err)
	}
	r.Item = workflow.Item{Kind: "issue", Number: 1, Repository: "acme/repo", URL: "https://github.com/acme/repo/issues/1", Title: "Issue"}
	r.Workflow.Task.IssueLinkMode = "reference"
	issue, err := workflowRunConfigWithLibrary(r, resolver)
	if err != nil || !strings.Contains(issue.Workflow.Instructions, "Library revision 3") {
		t.Fatal(issue, err)
	}
}
func TestLibraryScheduleMigrationPreservesTimingAndIsOneTime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	cfg := localruntime.InitConfig{Version: "v1", RuntimeConfigPath: path}
	cfg.ApplyDefaults()
	legacy := &localruntime.LibrarySchedules{Path: path, Init: cfg}
	saved, err := legacy.Save(localruntime.LibraryScheduleInput{LibraryTaskName: "unit-test-coverage", Repo: "https://github.com/acme/repo.git", IntervalHours: 12, BaseBranch: "feature", TargetSubdir: "src", Reviewers: []string{"reviewer"}, PromptContext: "Tests for API", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err = store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{}}); err != nil {
		t.Fatal(err)
	}
	if err = migrateLibrarySchedules(cfg, store); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Load()
	if len(doc.Workflows) != 1 || !doc.LibrarySchedulesMigrated {
		t.Fatal(doc)
	}
	w := doc.Workflows[0]
	if w.Trigger.Event != "schedule" || w.Trigger.IntervalHours != 12 || !w.Trigger.StartAt.Equal(saved.NextRunAt) || w.Task.LibraryTaskName != saved.LibraryTaskName || w.Task.Instructions != saved.PromptContext || w.Task.BaseBranch != "feature" || w.Task.TargetSubdir != "src" || !w.Task.Draft || !w.Enabled {
		t.Fatal(w)
	}
	doc.Workflows = nil
	store.Save(doc)
	if err = migrateLibrarySchedules(cfg, store); err != nil {
		t.Fatal(err)
	}
	doc, _ = store.Load()
	if len(doc.Workflows) != 0 {
		t.Fatal("deleted migration reappeared")
	}
	old, err := legacy.List()
	if err != nil || len(old) != 1 {
		t.Fatal("original schedules were not retained", err)
	}
}

func TestLibraryScheduleMigrationKeepsExplicitRootAndNoReviewers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := localruntime.InitConfig{Version: "v1", RuntimeConfigPath: filepath.Join(root, "config.json")}
	cfg.ApplyDefaults()
	legacy := &localruntime.LibrarySchedules{Path: cfg.RuntimeConfigPath, Init: cfg}
	if _, err := legacy.Save(localruntime.LibraryScheduleInput{LibraryTaskName: "unit-test-coverage", Repo: "acme/repo", IntervalHours: 24}); err != nil {
		t.Fatal(err)
	}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{}}); err != nil {
		t.Fatal(err)
	}
	if err := migrateLibrarySchedules(cfg, store); err != nil {
		t.Fatal(err)
	}
	doc, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	task := doc.Workflows[0].Task
	if task.TargetSubdir != "." || task.Reviewers == nil || len(*task.Reviewers) != 0 {
		t.Fatal("migration changed legacy root/reviewer defaults", task)
	}
}
