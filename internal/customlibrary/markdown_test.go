package customlibrary

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTaskFromMarkdownWithFrontmatter(t *testing.T) {
	t.Parallel()

	content := `---
displayName: Flaky Test Triage
type: review
icon: flask-conical
description: Investigate and stabilize flaky tests.
targetSubdir: services/api
commitMessage: "fix: stabilize flaky test"
labels: [tests, flaky]
reviewers:
  - alice
  - bob
requiresNonDefaultBranch: true
---

Investigate the flakiest test in CI and stabilize it.
`
	task, err := taskFromMarkdown("acme", "flaky-tests", content)
	if err != nil {
		t.Fatalf("taskFromMarkdown() error: %v", err)
	}
	if task.Name != "acme--flaky-tests" {
		t.Fatalf("Name = %q, want acme--flaky-tests", task.Name)
	}
	if task.DisplayName != "Flaky Test Triage" {
		t.Fatalf("DisplayName = %q", task.DisplayName)
	}
	if task.Icon != "flask-conical" {
		t.Fatalf("Icon = %q", task.Icon)
	}
	if task.TargetSubdir != "services/api" {
		t.Fatalf("TargetSubdir = %q", task.TargetSubdir)
	}
	if task.CommitMessage != "fix: stabilize flaky test" {
		t.Fatalf("CommitMessage = %q", task.CommitMessage)
	}
	if want := []string{"tests", "flaky"}; !equalStrings(task.Labels, want) {
		t.Fatalf("Labels = %v, want %v", task.Labels, want)
	}
	if want := []string{"alice", "bob"}; !equalStrings(task.Reviewers, want) {
		t.Fatalf("Reviewers = %v, want %v", task.Reviewers, want)
	}
	if !task.RequiresNonDefaultBranch {
		t.Fatalf("RequiresNonDefaultBranch = false, want true")
	}
	if task.Prompt != "Investigate the flakiest test in CI and stabilize it." {
		t.Fatalf("Prompt = %q", task.Prompt)
	}
}

func TestTaskFromMarkdownWithExplicitName(t *testing.T) {
	t.Parallel()

	content := "---\nname: custom-name\n---\nDo the thing.\n"
	task, err := taskFromMarkdown("acme", "filename-ignored", content)
	if err != nil {
		t.Fatalf("taskFromMarkdown() error: %v", err)
	}
	if task.Name != "acme--custom-name" {
		t.Fatalf("Name = %q, want acme--custom-name", task.Name)
	}
}

func TestTaskFromMarkdownWithoutFrontmatter(t *testing.T) {
	t.Parallel()

	task, err := taskFromMarkdown("acme", "Just A Prompt", "Do the whole thing end to end.\n")
	if err != nil {
		t.Fatalf("taskFromMarkdown() error: %v", err)
	}
	if task.Name != "acme--just-a-prompt" {
		t.Fatalf("Name = %q, want acme--just-a-prompt", task.Name)
	}
	if task.Prompt != "Do the whole thing end to end." {
		t.Fatalf("Prompt = %q", task.Prompt)
	}
}

func TestTaskFromMarkdownDefaultsDisplayNameToSourceName(t *testing.T) {
	t.Parallel()

	task, err := taskFromMarkdown("acme-docs", "fix-flaky-build", "Stabilize the build.\n")
	if err != nil {
		t.Fatalf("taskFromMarkdown() error: %v", err)
	}
	if want := "Acme Docs: Fix Flaky Build"; task.DisplayName != want {
		t.Fatalf("DisplayName = %q, want %q", task.DisplayName, want)
	}
}

func TestTaskFromMarkdownEmptyPromptRejected(t *testing.T) {
	t.Parallel()

	_, err := taskFromMarkdown("acme", "empty", "---\nname: empty\n---\n\n")
	if err == nil {
		t.Fatalf("taskFromMarkdown() = nil error, want error for empty prompt")
	}
}

func TestTaskFromMarkdownPromptContextRequiredWithoutLabelRejected(t *testing.T) {
	t.Parallel()

	content := "---\nname: needs-context\npromptContextRequired: true\n---\nDo the thing.\n"
	_, err := taskFromMarkdown("acme", "needs-context", content)
	if err == nil {
		t.Fatalf("taskFromMarkdown() = nil error, want error for promptContextRequired without promptContextLabel")
	}
}

func TestTaskFromMarkdownPromptContextPlaceholderWithoutLabelRejected(t *testing.T) {
	t.Parallel()

	content := "---\nname: needs-label\npromptContextPlaceholder: e.g. repro steps\n---\nDo the thing.\n"
	_, err := taskFromMarkdown("acme", "needs-label", content)
	if err == nil {
		t.Fatalf("taskFromMarkdown() = nil error, want error for promptContextPlaceholder without promptContextLabel")
	}
}

func TestTaskFromMarkdownPromptContextFieldsAccepted(t *testing.T) {
	t.Parallel()

	content := "---\nname: with-context\npromptContextLabel: Repro steps\npromptContextPlaceholder: e.g. steps to reproduce\npromptContextRequired: true\n---\nDo the thing.\n"
	task, err := taskFromMarkdown("acme", "with-context", content)
	if err != nil {
		t.Fatalf("taskFromMarkdown() error: %v", err)
	}
	if task.PromptContextLabel != "Repro steps" {
		t.Fatalf("PromptContextLabel = %q", task.PromptContextLabel)
	}
	if task.PromptContextPlaceholder != "e.g. steps to reproduce" {
		t.Fatalf("PromptContextPlaceholder = %q", task.PromptContextPlaceholder)
	}
	if !task.PromptContextRequired {
		t.Fatalf("PromptContextRequired = false, want true")
	}
}

func TestLoadPromptMarkdownNamespacesAndSorts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b-task.md"), "---\nname: b\n---\nSecond prompt.\n")
	writeFile(t, filepath.Join(dir, "a-task.md"), "---\nname: a\n---\nFirst prompt.\n")
	writeFile(t, filepath.Join(dir, "README.txt"), "not a task")

	tasks, err := loadPromptMarkdown(dir, "acme")
	if err != nil {
		t.Fatalf("loadPromptMarkdown() error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if tasks[0].Name != "acme--a" || tasks[1].Name != "acme--b" {
		t.Fatalf("tasks = %+v, want sorted a then b", tasks)
	}
}

func TestLoadPromptMarkdownRejectsDuplicateNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "one.md"), "---\nname: dup\n---\nPrompt one.\n")
	writeFile(t, filepath.Join(dir, "two.md"), "---\nname: dup\n---\nPrompt two.\n")

	if _, err := loadPromptMarkdown(dir, "acme"); err == nil {
		t.Fatalf("loadPromptMarkdown() = nil error, want duplicate name error")
	}
}

func TestLoadPromptMarkdownRequiresAtLeastOneFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, err := loadPromptMarkdown(dir, "acme"); err == nil {
		t.Fatalf("loadPromptMarkdown() = nil error, want error for empty directory")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
