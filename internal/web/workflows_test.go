package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/workflow"
)

func TestWorkflowAPIStoreAndPreview(t *testing.T) {
	t.Parallel()
	srv := NewServer("", NewBroker())
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	calls := 0
	srv.Workflows = &workflow.Engine{Store: store, Allowed: func(repo string) bool { return repo != "acme/disabled" }, Fetch: func(context.Context, string) ([]workflow.Item, error) {
		calls++
		return []workflow.Item{{Number: 1, Kind: "issue", Repository: "acme/repo", Open: true}}, nil
	}}
	h := srv.Handler()
	request := func(method, path string, data any) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(data)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(body)))
		return rec
	}
	rec := request("GET", "/api/workflows", nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	var d workflow.Document
	json.Unmarshal(rec.Body.Bytes(), &d)
	w := workflow.Definition{ID: "one", Name: "One", Repository: "acme/repo", Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all", Rules: []workflow.Rule{}}, Task: workflow.Task{Kind: "implement", Instructions: "Implement", IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash"}}
	d.Workflows = append(d.Workflows, w)
	rec = request("PUT", "/api/workflows", d)
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if stale := request("PUT", "/api/workflows", d); stale.Code != 409 {
		t.Fatalf("stale write %d", stale.Code)
	}
	rec = request("POST", "/api/workflows/preview", w)
	if rec.Code != 200 || calls != 1 {
		t.Fatal(rec.Body, calls)
	}
	var decisions []workflow.Decision
	json.Unmarshal(rec.Body.Bytes(), &decisions)
	if len(decisions) != 1 || !decisions[0].Matches {
		t.Fatal(decisions)
	}
	w.Repository = "acme/disabled"
	rec = request("POST", "/api/workflows/preview", w)
	if rec.Code != 400 || calls != 1 {
		t.Fatal("preview bypassed repo gate")
	}
	d.Version = 99
	if rec = request("PUT", "/api/workflows", d); rec.Code != 400 {
		t.Fatal(rec.Body)
	}
}
