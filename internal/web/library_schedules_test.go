package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The Library shortcut opens a scheduled Workflow draft with the selected options.
func TestLibrarySchedulingMovesToWorkflowInBrowser(t *testing.T) {
	requireBrowserTooling(t, "Library to Workflow scheduling")

	server := NewServer("", NewBroker())
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	runBrowserSuite(t, ts.URL, 60*time.Second, "Library to Workflow scheduling", "tests/library_schedules.cjs")
}
