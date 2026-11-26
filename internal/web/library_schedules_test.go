package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// The Library shortcut opens a scheduled Workflow draft with the selected options.
func TestLibrarySchedulingMovesToWorkflowInBrowser(t *testing.T) {
	requireBrowserTooling(t, "Library to Workflow scheduling")

	server := NewServer("", NewBroker())
	server.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		return nil, errors.New("repository data is supplied by the browser fixture")
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	runBrowserSuite(t, ts.URL, 60*time.Second, "Library to Workflow scheduling", "tests/library_schedules.cjs")
}
