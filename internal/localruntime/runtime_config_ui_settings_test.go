package localruntime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadRuntimeConfigUISettingsMatchesIndividualReaders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	paths := map[string]string{
		"empty path": "",
		"missing":    filepath.Join(dir, "missing.json"),
		"invalid":    write("invalid.json", "{"),
		"null":       write("null.json", "null"),
		"defaults":   write("defaults.json", "{}"),
		"custom": write("custom.json", `{
			"default_library_enabled": false,
			"chat_enabled": true,
			"json_enabled": true,
			"github_issues_enabled": true,
			"github_issue_link_mode": "close",
			"github_issue_auto_start_assigned": true,
			"github_issue_auto_start_only_assigned": false,
			"github_issue_auto_start_labels": ["bug", " agent ", "bug"],
			"assign_creator": false
		}`),
	}
	for name, path := range paths {
		want := RuntimeConfigUISettings{
			DefaultLibraryEnabled:            ReadRuntimeConfigDefaultLibraryEnabled(path),
			ChatEnabled:                      ReadRuntimeConfigChatEnabled(path),
			JSONEnabled:                      ReadRuntimeConfigJSONEnabled(path),
			GitHubIssuesEnabled:              ReadRuntimeConfigGitHubIssuesEnabled(path),
			GitHubIssueLinkMode:              ReadRuntimeConfigGitHubIssueLinkMode(path),
			GitHubIssueAutoStartAssigned:     ReadRuntimeConfigGitHubIssueAutoStartAssigned(path),
			GitHubIssueAutoStartOnlyAssigned: ReadRuntimeConfigGitHubIssueAutoStartOnlyAssigned(path),
			GitHubIssueAutoStartLabels:       ReadRuntimeConfigGitHubIssueAutoStartLabels(path),
			AssignCreator:                    ReadRuntimeConfigAssignCreator(path),
		}
		if got := ReadRuntimeConfigUISettings(path); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ReadRuntimeConfigUISettings() = %+v, want %+v", name, got, want)
		}
	}
}
