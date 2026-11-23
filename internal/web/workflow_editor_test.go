package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkflowEditorInBrowser(t *testing.T) {
	requireBrowserTooling(t, "Workflow editor")
	srv := NewServer("", NewBroker())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	runBrowserSuite(t, ts.URL, time.Minute, "Workflow editor", "tests/workflow_editor.cjs")
}
