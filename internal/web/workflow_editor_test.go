package web

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestWorkflowEditorInBrowser(t *testing.T) {
	requireBrowserTooling(t, "Workflow editor")
	srv := NewServer("", NewBroker())
	srv.Workflows = &workflow.Engine{Store: &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	runBrowserSuite(t, ts.URL, time.Minute, "Workflow editor", "tests/workflow_editor.cjs")
}
