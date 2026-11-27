package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestResumePublishedTaskKeepsDefaultPolicyAndOriginalPR(t *testing.T) {
	t.Parallel()
	policy := workflow.ExecutionPolicy{ReviewCycles: 10, ReviewAgent: config.ReviewAgentConfig{Complexity: "easy"}}
	captured := policy.Snapshot()
	cfg := config.Config{RepoURL: "https://github.com/acme/repo", Workflow: captured}
	task := web.Task{Status: "completed", ReviewFindingsRemain: true, PRURL: "https://github.com/acme/repo/pull/7"}
	if !resumePublishedTask(task, &cfg) {
		t.Fatal("published draft was not eligible for in-place review retry")
	}
	id, err := retryWorkflowTask(t.Context(), nil, task, cfg, func(run config.Config) (string, error) {
		if run.Workflow.ResumePRURL != task.PRURL || run.Workflow.ReviewCycles != 10 ||
			run.Workflow.ReviewAgent.Complexity != "easy" || run.Workflow.PolicyHash != captured.PolicyHash {
			t.Fatalf("retry changed the published target or captured policy: %+v", run.Workflow)
		}
		return "existing-task", nil
	})
	if err != nil || id != "existing-task" || captured.ResumePRURL != "" {
		t.Fatalf("retry = %q, %v, original policy = %+v", id, err, captured)
	}
}

func TestResumePublishedMultiRepositoryTaskFromLogs(t *testing.T) {
	t.Parallel()
	const first = "https://github.com/acme/one/pull/1"
	const second = "https://github.com/acme/two/pull/2"
	broker := web.NewBroker()
	broker.IngestLog("task status=start request_id=existing-task explicit_terminal=true")
	broker.IngestLog("task request_id=existing-task stage=pr status=ok pr_url=" + first)
	broker.IngestLog("task request_id=existing-task stage=pr status=ok pr_url=" + second)
	broker.IngestLog("task request_id=existing-task stage=review status=start action=pass pr_url=" + first)
	broker.IngestLog("task status=error request_id=existing-task pr_url=" + first)
	task, ok := broker.Task("existing-task")
	policy := workflow.ExecutionPolicy{ReviewCycles: 10, ReviewAgent: config.ReviewAgentConfig{Complexity: "easy"}}
	cfg := config.Config{Repos: []string{"git@github.com:acme/one.git", "https://github.com/acme/two", "https://github.com/acme/unchanged"}, Workflow: policy.Snapshot()}
	if !ok || !resumePublishedTask(task, &cfg) || !slices.Equal(cfg.Workflow.ResumePRURLs, []string{first, second}) {
		t.Fatalf("retry lost published PR targets: task=%+v policy=%+v", task, cfg.Workflow)
	}
}

func TestReviewRetryRetainsCapturedCustomPolicyWithoutLiveActivation(t *testing.T) {
	t.Parallel()
	captured := workflow.ExecutionPolicy{ReviewCycles: 10, ReviewAgent: config.ReviewAgentConfig{Complexity: "easy"}}
	original := workflow.Definition{ID: "input", Name: "Input", Repository: "acme/repo", Enabled: true,
		Trigger: workflow.Trigger{Event: "app.prompt"}, Filters: workflow.Filters{Mode: "all"},
		Task: workflow.Task{Kind: "implement", IssueLinkMode: "reference"}, Merge: workflow.Merge{Enabled: true, Method: "squash"}}
	for _, change := range []string{"edited", "removed", "paused"} {
		t.Run(change, func(t *testing.T) {
			doc := workflow.Document{Version: workflow.Version}
			current := original
			switch change {
			case "edited":
				current.Task.Instructions = "New instructions"
			case "paused":
				current.Enabled = false
			}
			if change != "removed" {
				doc.Workflows = []workflow.Definition{current}
			}
			root := t.TempDir()
			store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
			if err := store.Initialize(doc); err != nil {
				t.Fatal(err)
			}
			engine := &workflow.Engine{Store: store, StatePath: filepath.Join(root, "state.json"),
				Enabled: func() bool { return true }, Allowed: func(string) bool { return true }}
			policy := captured.Snapshot()
			policy.ID, policy.EventID, policy.Name = original.ID, "original", original.Name
			policy.PolicyHash = original.ExecutionPolicyHash()
			cfg := config.Config{RepoURL: "https://github.com/acme/repo", Workflow: policy}
			task := web.Task{Status: "error", PRURL: "https://github.com/acme/repo/pull/7"}
			if !resumePublishedTask(task, &cfg) {
				t.Fatal("published task was not eligible for repair")
			}
			want := *cfg.Workflow
			want.DetachedReview = true
			id, err := retryWorkflowTask(t.Context(), engine, task, cfg, func(run config.Config) (string, error) {
				if run.Workflow == nil || run.Workflow.ID != want.ID || run.Workflow.EventID != want.EventID ||
					run.Workflow.PolicyHash != want.PolicyHash || run.Workflow.ResumePRURL != want.ResumePRURL ||
					run.Workflow.ReviewCycles != want.ReviewCycles || run.Workflow.ReviewAgent != want.ReviewAgent || !run.Workflow.DetachedReview {
					t.Fatalf("captured review retry = %+v", run.Workflow)
				}
				return "existing-task", nil
			})
			if err != nil || id != "existing-task" {
				t.Fatalf("retry = %q, %v", id, err)
			}
			if runs, err := engine.Runs(); err != nil || len(runs) != 0 {
				t.Fatalf("detached retry scheduled workflow actions: %v, %v", runs, err)
			}
		})
	}
}

func TestWorkflowRunConfigPolicies(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"implement", "review", "repair"} {
		t.Run(kind, func(t *testing.T) {
			w := workflow.Definition{ID: "one", Name: "Workflow", Repository: "acme/repo", ReviewCycles: 3, Task: workflow.Task{Complexity: "advanced", Kind: kind, Instructions: "Do the work", IssueLinkMode: "close", PRAssignee: "maintainer", PRLabels: []string{"tracking"}}}
			w.ReviewAgent = config.ReviewAgentConfig{Complexity: "easy", Instructions: "Check for regressions"}
			i := workflow.Item{Number: 12, Kind: "issue", URL: "https://github.com/acme/repo/issues/12", Title: "A task", HeadBranch: "feature"}
			if kind != "implement" {
				i.Kind = "pr"
				i.URL = "https://github.com/acme/repo/pull/12"
			}
			c, err := workflowRunConfig(workflow.Run{ID: "event", Workflow: w, Item: i})
			if err != nil {
				t.Fatal(err)
			}
			if c.Complexity != "advanced" || c.Workflow.ReviewCycles != 3 || c.Workflow.ReviewAgent != w.ReviewAgent || c.Workflow.EventID != "event" || *c.AssignCreator {
				t.Fatal(c)
			}
			if c.Workflow.PRAssignee != w.Task.PRAssignee || !slices.Equal(c.Workflow.PRLabels, w.Task.PRLabels) {
				t.Fatalf("lost PR routing policy: %+v", c.Workflow)
			}
			if kind == "review" && (c.Review == nil || c.Review.AutoMerge || c.Review.Writeback != "summary-comment") {
				t.Fatal("review must report outcome before merge", c)
			}
			if kind == "repair" && (c.BaseBranch != "feature" || !c.RequiresNonDefaultBranch || c.Workflow.RepairPRURL != i.URL) {
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
func TestActionFailureRunConfigIncludesDiagnosisContext(t *testing.T) {
	t.Parallel()
	w := workflow.Definition{
		ID: "release", Name: "Repair release", Repository: "acme/repo",
		Trigger: workflow.Trigger{Event: workflow.ActionFailedEvent, Value: "deploy.yml"},
		Task:    workflow.Task{Kind: "implement", LibraryTaskName: "diagnose", BaseBranch: "release"},
	}
	i := workflow.Item{
		Kind: "action", ActionRunID: 123, ActionRunAttempt: 2,
		URL: "https://github.com/acme/repo/actions/runs/123/attempts/2", HeadBranch: "v1.2.3", HeadSHA: "abc",
	}
	c, err := workflowRunConfigWithLibrary(workflow.Run{ID: "event", Workflow: w, Item: i}, func(name, repo, branch, _ string) (config.Config, error) {
		return config.Config{RepoURL: repo, BaseBranch: branch, LibraryTaskName: name, Prompt: "Diagnose release failures."}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseBranch != "release" || c.GitHubIssue != nil || c.Review != nil || c.Workflow.RepairPRURL != "" {
		t.Fatalf("expected a repository implementation task on the configured base: %+v", c)
	}
	for _, want := range []string{"Diagnose release failures.", i.URL, "deploy.yml", "Run ID: 123", "Attempt: 2", "Head branch: v1.2.3", "Head commit: abc"} {
		if !strings.Contains(c.Prompt, want) {
			t.Errorf("prompt missing %q: %s", want, c.Prompt)
		}
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

func TestConfigureWorkflowsNeverImportsLegacyConfiguration(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			root := t.TempDir()
			cfg := localruntime.InitConfig{Version: "v1", RuntimeConfigPath: filepath.Join(root, "config.json")}
			cfg.ApplyDefaults()
			original := []byte(`{"workflow_enabled":true,"github_issues_enabled":true,"github_issue_repositories":{"acme/repo":{"auto_start":true,"trigger_labels":["ready"],"exclusion_label":"agent_00-in-progress"}},"review_watch":{"enabled":true,"review_level":"high","auto_merge":true},"library_schedules":{"unit-test-coverage@acme/repo":{"libraryTaskName":"unit-test-coverage","repo":"acme/repo","intervalHours":24,"nextRunAt":"2026-01-01T00:00:00Z"}}}`)
			if err := os.WriteFile(cfg.RuntimeConfigPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(cfg.RuntimeConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
			if existing {
				w := workflow.Definition{ID: "mine", Name: "My workflow", Repository: "acme/repo", Trigger: workflow.Trigger{Event: "app.prompt"}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash"}}
				if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
					t.Fatal(err)
				}
			}
			server := web.NewServer("", web.NewBroker())
			for range 2 {
				if err := configureWorkflows(context.Background(), cfg, nil, &server, server.Broker, nil, func(string, ...any) {}); err != nil {
					t.Fatal(err)
				}
			}
			server.Workflows.Fetch = func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) {
				t.Fatal("startup scanned GitHub without an enabled workflow")
				return nil, nil
			}
			if err := server.Workflows.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			doc, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if existing {
				want = 1
			}
			if len(doc.Workflows) != want || (existing && doc.Workflows[0].ID != "mine") {
				t.Fatal("startup created or replaced workflows", doc)
			}
			after, err := os.ReadFile(cfg.RuntimeConfigPath)
			if err != nil || string(after) != string(before) {
				t.Fatal("startup changed legacy settings", err)
			}
		})
	}
}

func TestWorkflowMetadataRetryKeepsOriginalPullRequest(t *testing.T) {
	t.Parallel()
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	w := workflow.Definition{ID: "input", Name: "Input", Repository: "acme/repo", Trigger: workflow.Trigger{Event: "app.prompt"},
		Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", IssueLinkMode: "reference", PRAssignee: "maintainer"},
		Merge: workflow.Merge{Enabled: true, Method: "squash"}}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	engine := &workflow.Engine{Store: store, StatePath: filepath.Join(t.TempDir(), "state.json")}
	cfg := config.Config{RepoURL: "https://github.com/acme/repo", Workflow: &config.WorkflowRunConfig{
		ID: w.ID, EventID: "original", PolicyHash: w.ExecutionPolicyHash(), PRAssignee: w.Task.PRAssignee,
	}}
	task := web.Task{Status: "error", PRURL: "https://github.com/acme/repo/pull/42",
		Error: "pr: workflow PR metadata update failed: GitHub did not apply assignee"}
	id, err := retryWorkflowTask(t.Context(), engine, task, cfg, func(retry config.Config) (string, error) {
		if retry.Workflow.ResumePRURL != task.PRURL || retry.Workflow.PolicyHash != cfg.Workflow.PolicyHash ||
			retry.Workflow.EventID == cfg.Workflow.EventID || retry.Workflow.PRAssignee != cfg.Workflow.PRAssignee {
			t.Fatalf("retry lost target or policy: %+v", retry.Workflow)
		}
		return "retry-request", nil
	})
	if err != nil || id != "retry-request" || cfg.Workflow.ResumePRURL != "" {
		t.Fatalf("retry = %q, %v, original policy = %+v", id, err, cfg.Workflow)
	}
}

func TestStoppedWorkflowMetadataRetryKeepsPersistedPullRequest(t *testing.T) {
	t.Parallel()
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	w := workflow.Definition{ID: "input", Name: "Input", Repository: "acme/repo", Trigger: workflow.Trigger{Event: "app.prompt"},
		Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", IssueLinkMode: "reference", PRAssignee: "maintainer"},
		Merge: workflow.Merge{Enabled: true, Method: "squash"}}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	engine := &workflow.Engine{Store: store, StatePath: filepath.Join(t.TempDir(), "state.json")}
	cfg := config.Config{RepoURL: "https://github.com/acme/repo", Workflow: &config.WorkflowRunConfig{
		ID: w.ID, EventID: "original", PolicyHash: w.ExecutionPolicyHash(), PRAssignee: w.Task.PRAssignee,
	}}
	cfg.Workflow.ResumePRURL = "https://github.com/acme/repo/pull/42"
	task := web.Task{Status: "stopped", PRURL: "https://github.com/acme/repo/pull/42",
		Error: "operator stopped task"}
	id, err := retryWorkflowTask(t.Context(), engine, task, cfg, func(retry config.Config) (string, error) {
		if retry.Workflow.ResumePRURL != task.PRURL || retry.Workflow.PolicyHash != cfg.Workflow.PolicyHash ||
			retry.Workflow.EventID == cfg.Workflow.EventID || retry.Workflow.PRAssignee != cfg.Workflow.PRAssignee {
			t.Fatalf("retry lost target or policy: %+v", retry.Workflow)
		}
		return "retry-request", nil
	})
	if err != nil || id != "retry-request" || cfg.Workflow.ResumePRURL != task.PRURL {
		t.Fatalf("retry = %q, %v, original policy = %+v", id, err, cfg.Workflow)
	}
}
