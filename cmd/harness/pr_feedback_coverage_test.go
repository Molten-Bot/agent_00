package main

import (
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/web"
)

func TestPRReviewFeedbackRunConfigRequiresRepository(t *testing.T) {
	t.Parallel()

	_, err := prReviewFeedbackRunConfig(config.Config{}, web.Task{}, web.PRReviewFeedback{})
	if err == nil || !strings.Contains(err.Error(), "no repository") {
		t.Fatalf("prReviewFeedbackRunConfig(no repos) error = %v, want no-repository error", err)
	}
}

func TestPRReviewFeedbackRunConfigRequiresHeadBranch(t *testing.T) {
	t.Parallel()

	original := config.Config{
		RepoURL: "git@github.com:acme/repo.git",
		Prompt:  "do work",
	}
	_, err := prReviewFeedbackRunConfig(original, web.Task{}, web.PRReviewFeedback{})
	if err == nil || !strings.Contains(err.Error(), "head branch is required") {
		t.Fatalf("prReviewFeedbackRunConfig(no head branch) error = %v, want head-branch error", err)
	}
}

func TestPRReviewFeedbackRunConfigDefaultsTitleFromFeedbackOrTask(t *testing.T) {
	t.Parallel()

	original := config.Config{
		RepoURL:    "git@github.com:acme/repo.git",
		BaseBranch: "main",
		Prompt:     "do work",
		PRTitle:    "",
	}
	original.ApplyDefaults()
	original.PRTitle = ""

	task := web.Task{Branch: "agent00-feature", Prompt: "fallback title from task"}
	feedback := web.PRReviewFeedback{Title: "feedback title"}

	cfg, err := prReviewFeedbackRunConfig(original, task, feedback)
	if err != nil {
		t.Fatalf("prReviewFeedbackRunConfig() error = %v", err)
	}
	if got, want := cfg.PRTitle, "feedback title"; got != want {
		t.Fatalf("cfg.PRTitle = %q, want %q (feedback title preferred)", got, want)
	}
}

func TestPRReviewFeedbackRunConfigPropagatesValidationFailure(t *testing.T) {
	t.Parallel()

	original := config.Config{
		RepoURL:      "git@github.com:acme/repo.git",
		BaseBranch:   "main",
		Prompt:       "do work",
		AgentHarness: "not-a-real-harness",
	}

	_, err := prReviewFeedbackRunConfig(original, web.Task{}, web.PRReviewFeedback{})
	if err == nil || !strings.Contains(err.Error(), "validate review-feedback run config") {
		t.Fatalf("prReviewFeedbackRunConfig(invalid harness) error = %v, want validation wrap error", err)
	}
}

func TestWritePromptFieldSkipsEmptyValue(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writePromptField(&b, "Label", "   ")
	if got := b.String(); got != "" {
		t.Fatalf("writePromptField(empty value) wrote %q, want nothing", got)
	}

	writePromptField(&b, "Label", " value ")
	if got, want := b.String(), "- Label: value\n"; got != want {
		t.Fatalf("writePromptField() = %q, want %q", got, want)
	}
}

func TestNonEmptyIndentedUsesFallbackForBlankValue(t *testing.T) {
	t.Parallel()

	got := nonEmptyIndented("   ", "(fallback)")
	if want := "  (fallback)"; got != want {
		t.Fatalf("nonEmptyIndented(blank) = %q, want %q", got, want)
	}

	got = nonEmptyIndented("line one\nline two", "(fallback)")
	if want := "  line one\n  line two"; got != want {
		t.Fatalf("nonEmptyIndented(multi-line) = %q, want %q", got, want)
	}
}
