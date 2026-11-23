package web

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWorkflowDockRequiresOptIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		enabled  bool
		err      error
		noStatus bool
	}{
		{name: "default", noStatus: true},
		{name: "disabled"},
		{name: "enabled", enabled: true},
		{name: "unavailable", enabled: true, err: errors.New("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("", NewBroker())
			if !tc.noStatus {
				s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
					return ReviewSettingsState{WorkflowEnabled: tc.enabled}, tc.err
				}
			}
			dock := string(s.injectBottomDockComponent(context.Background(), []byte(bottomDockPlaceholder)))
			hidden := strings.Contains(dock, `id="workflow-dock-link" class="prompt-mode-link site-dock-link hidden"`)
			if want := !tc.enabled || tc.err != nil; hidden != want {
				t.Fatalf("Workflow hidden = %v, want %v", hidden, want)
			}
			if strings.Count(dock, `id="workflow-dock-link"`) != 1 {
				t.Fatal("expected exactly one Workflow link")
			}
		})
	}
}
