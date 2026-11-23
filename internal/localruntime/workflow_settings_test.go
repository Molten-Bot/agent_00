package localruntime

import (
	"path/filepath"
	"testing"
)

func TestWorkflowSettingsDefaultAndPersistence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if ReadRuntimeConfigUISettings(path).WorkflowEnabled {
		t.Fatal("Workflow must default to off")
	}
	for _, enabled := range []bool{true, false} {
		err := SaveRuntimeConfigReviewDefaultLibraryAndGitHubIssuesSettings(path, InitConfig{}, ReviewWatchConfig{}, nil, nil, nil, nil, nil, nil, nil, RuntimeConfigUISettingsUpdate{WorkflowEnabled: &enabled})
		if err != nil {
			t.Fatal(err)
		}
		if got := ReadRuntimeConfigUISettings(path).WorkflowEnabled; got != enabled {
			t.Fatalf("Workflow = %v, want %v", got, enabled)
		}
		// Older clients omitting the toggle must preserve its saved value.
		if err := SaveRuntimeConfigReviewDefaultLibraryAndGitHubIssuesSettings(path, InitConfig{}, ReviewWatchConfig{}, nil, nil, nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if got := ReadRuntimeConfigUISettings(path).WorkflowEnabled; got != enabled {
			t.Fatalf("omitted Workflow changed to %v", got)
		}
	}
}
