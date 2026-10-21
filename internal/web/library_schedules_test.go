package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestLibraryScheduleAPI(t *testing.T) {
	t.Parallel()
	server := NewServer("", NewBroker())
	server.LibrarySchedules = &localruntime.LibrarySchedules{Path: filepath.Join(t.TempDir(), "config.json"), Init: localruntime.InitConfig{Version: "v1"}}
	handler := server.Handler()
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s: %d want %d: %s", method, w.Code, want, w.Body)
		}
		return w
	}
	path := "/api/library/schedules"
	body := `{"libraryTaskName":"unit-test-coverage","repo":"acme/repo","intervalHours":24}`
	call(http.MethodPut, path, body, http.StatusOK)
	call(http.MethodPut, path, strings.ReplaceAll(body, `"intervalHours":24`, `"intervalHours":6`), http.StatusOK)
	w := call(http.MethodGet, path, "", http.StatusOK)
	var result struct {
		Schedules []localruntime.LibrarySchedule `json:"schedules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Schedules) != 1 || result.Schedules[0].IntervalHours != 6 {
		t.Fatalf("update duplicated or lost: %+v", result)
	}
	call(http.MethodPut, path, body+body, http.StatusBadRequest)
	call(http.MethodPut, path, `{"libraryTaskName":"missing","repo":"acme/repo","intervalHours":24}`, http.StatusBadRequest)
	call(http.MethodPut, path, `{"libraryTaskName":"unit-test-coverage","repo":"acme/repo","intervalHours":24,"prompt":"override"}`, http.StatusBadRequest)
	call(http.MethodPost, path, body, http.StatusMethodNotAllowed)
	call(http.MethodDelete, path, "", http.StatusBadRequest)
	call(http.MethodDelete, path+"?key="+url.QueryEscape(result.Schedules[0].Key), "", http.StatusOK)
	w = call(http.MethodGet, path, "", http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Schedules) != 0 {
		t.Fatal("schedule survived delete")
	}
}
