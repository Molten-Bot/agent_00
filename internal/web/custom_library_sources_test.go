package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/customlibrary"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

type fakeCustomLibrarySyncer struct {
	err error
}

func (f fakeCustomLibrarySyncer) Sync(_ context.Context, src customlibrary.Source) (customlibrary.SyncResult, error) {
	if f.err != nil {
		return customlibrary.SyncResult{}, f.err
	}
	return customlibrary.SyncResult{Source: src, CommitSHA: "deadbeef", TaskCount: 2, Changed: true}, nil
}

func (f fakeCustomLibrarySyncer) RemoveSource(string) error { return nil }

func TestCustomLibrarySourcesAPI(t *testing.T) {
	t.Parallel()
	broker := NewBroker()
	server := NewServer("", broker)
	server.CustomLibrarySources = &localruntime.CustomLibrarySources{
		Path:   filepath.Join(t.TempDir(), "config.json"),
		Init:   localruntime.InitConfig{Version: "v1"},
		Syncer: fakeCustomLibrarySyncer{},
		OnChanged: func() {
			broker.NotifyCustomLibraryUpdated()
		},
	}
	handler := server.Handler()

	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, want, w.Body)
		}
		return w
	}

	path := "/api/library/sources"
	beforeGeneration := broker.Snapshot().CustomLibraryGeneration

	call(http.MethodPost, path, `{"name":"Acme","repo":"git@github.com:acme/docs.git"}`, http.StatusOK)

	w := call(http.MethodGet, path, "", http.StatusOK)
	var result struct {
		Sources []localruntime.CustomLibrarySource `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 || result.Sources[0].Name != "acme" || result.Sources[0].TaskCount != 2 {
		t.Fatalf("unexpected sources: %+v", result.Sources)
	}

	if after := broker.Snapshot().CustomLibraryGeneration; after != beforeGeneration+1 {
		t.Fatalf("CustomLibraryGeneration = %d, want %d", after, beforeGeneration+1)
	}

	call(http.MethodPost, path, `{"name":"","repo":"x"}`, http.StatusBadRequest)
	call(http.MethodPost, path, `{"name":"acme","repo":"x","extra":true}`, http.StatusBadRequest)
	call(http.MethodPut, path, "", http.StatusMethodNotAllowed)
	call(http.MethodDelete, path, "", http.StatusBadRequest)
	call(http.MethodDelete, path+"?name="+url.QueryEscape("acme"), "", http.StatusOK)

	w = call(http.MethodGet, path, "", http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 0 {
		t.Fatalf("source survived delete: %+v", result.Sources)
	}
}

func TestCustomLibrarySourcesAPIUnavailable(t *testing.T) {
	t.Parallel()
	server := NewServer("", NewBroker())
	handler := server.Handler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/library/sources", nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestCustomLibrarySourcesAPIAddFailureNotPersisted(t *testing.T) {
	t.Parallel()
	server := NewServer("", NewBroker())
	server.CustomLibrarySources = &localruntime.CustomLibrarySources{
		Path:   filepath.Join(t.TempDir(), "config.json"),
		Init:   localruntime.InitConfig{Version: "v1"},
		Syncer: fakeCustomLibrarySyncer{err: errors.New("permission denied")},
	}
	handler := server.Handler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/library/sources", strings.NewReader(`{"name":"acme","repo":"git@github.com:acme/docs.git"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body)
	}
}
