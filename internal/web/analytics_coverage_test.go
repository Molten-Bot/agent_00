package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestSettingsAndIssueAssetsTrackMeaningfulEngagement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		asset  string
		events []string
	}{
		{
			asset: "static/commit-signing.js",
			events: []string{
				"commit_signing_load_failed",
				"commit_signing_save_started",
				"commit_signing_save_succeeded",
				"commit_signing_save_failed",
				"commit_signing_verify_started",
				"commit_signing_verify_succeeded",
				"commit_signing_verify_failed",
				"commit_signing_public_key_copied",
				"commit_signing_public_key_copy_failed",
			},
		},
		{
			asset: "static/custom-library-sources.js",
			events: []string{
				"custom_library_sources_load_failed",
				"custom_library_source_validation_failed",
				"custom_library_source_add_started",
				"custom_library_source_add_succeeded",
				"custom_library_source_add_failed",
				"custom_library_source_remove_started",
				"custom_library_source_remove_succeeded",
				"custom_library_source_remove_failed",
				"custom_library_sources_refresh_failed",
			},
		},
		{
			asset: "static/library-schedules.js",
			events: []string{
				"library_schedules_opened",
				"library_schedules_load_failed",
				"library_schedule_editor_opened",
				"library_schedule_validation_failed",
				"library_schedule_${operation}_started",
				"library_schedule_${operation}_succeeded",
				"library_schedule_${operation}_failed",
				"library_schedule_run_now_started",
				"library_schedule_run_now_succeeded",
				"library_schedule_run_now_failed",
				"library_schedules_refresh_failed",
			},
		},
		{
			asset: "static/github-issues.js",
			events: []string{
				"github_issue_opened_external",
				"github_issue_task_validation_failed",
				"github_issue_task_started",
				"github_issue_task_succeeded",
				"github_issue_task_duplicate",
				"github_issue_task_failed",
				"github_issue_selected",
				"github_issue_load_failed",
				"github_issues_loaded",
				"github_issues_load_failed",
				"github_issues_initialization_failed",
				"github_issues_repo_selected",
				"github_issues_assignee_filter_changed",
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.asset, func(t *testing.T) {
			t.Parallel()
			contents, err := fs.ReadFile(staticFiles, test.asset)
			if err != nil {
				t.Fatalf("read %s: %v", test.asset, err)
			}
			script := string(contents)
			if !strings.Contains(script, `typeof window.trackAnalyticsEvent`) {
				t.Fatalf("%s does not route events through the shared analytics helper", test.asset)
			}
			for _, event := range test.events {
				if !strings.Contains(script, event) {
					t.Errorf("%s missing analytics event %q", test.asset, event)
				}
			}
		})
	}
}
