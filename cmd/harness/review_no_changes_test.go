package main

import (
	"fmt"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/library"
)

func TestMissingPRFeedbackTargetDoesNotQueueAnotherPR(t *testing.T) {
	t.Parallel()
	result := app.Result{Err: fmt.Errorf("resolve target: %w", app.ErrPRFeedbackTarget), ExitCode: app.ExitPR}
	if queue, reason := shouldQueueFailureFollowUp(localSubmitSource, result); queue {
		t.Fatalf("queued repair for missing target: %s", reason)
	}
}

func TestReadOnlyReviewNoChangesDoesNotQueueRepairs(t *testing.T) {
	t.Parallel()
	catalog, err := library.LoadCatalog("../../library")
	if err != nil {
		t.Fatal(err)
	}
	task, ok := catalog.Task("code-review")
	if !ok {
		t.Fatal("code-review task missing")
	}
	for _, tc := range []struct {
		name string
		cfg  config.Config
	}{
		{"structured review", config.Config{Review: &config.ReviewConfig{PRNumber: 77}, Prompt: "Find issues the author can fix"}},
		{"library review", config.Config{LibraryTaskName: "code-review", Prompt: "Find issues the author can fix"}},
		{"expanded review prompt", config.Config{Prompt: task.Prompt + "\n\n" + failurefollowup.ExecutionContract}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := app.Result{NoChanges: true, ExitCode: app.ExitSuccess}
			if queue, reason := shouldQueueUnexpectedNoChangesFollowUp(result, tc.cfg); queue {
				t.Fatalf("queued review repair: %s", reason)
			}
			if escalate, reason := shouldEscalateNoChangesFollowUp(noChangesFollowUpSource, result, tc.cfg); escalate {
				t.Fatalf("escalated review: %s", reason)
			}
			if failed, changed := localNoChangesFailureResult(localSubmitSource, result, tc.cfg); changed {
				t.Fatalf("review became failure: %+v", failed)
			}
		})
	}
}

func TestQuotedReadOnlyReviewDoesNotSuppressImplementationFailure(t *testing.T) {
	t.Parallel()
	for _, prompt := range []string{
		"Fix the comments in the review",
		failurefollowup.RequiredPrompt + "\n\nOriginal task prompt:\nThis is a review-only task: do not edit files.",
		"Implement review-only task detection and add regression tests",
	} {
		if !promptRequestsRepositoryChange(prompt) {
			t.Fatalf("implementation prompt classified as read-only: %q", prompt)
		}
	}
}
