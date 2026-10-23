package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestLibraryScheduleSliderSnapsToStepsInBrowser drives the real
// static/library-schedules.js in a headless browser, unlike TestLibraryScheduleAPI
// above which only exercises the Go-side interval validation. hoursForIndex and
// indexForHours are unexported closures with no Go-reachable equivalent, so this
// is the only coverage for the slider's single-digit -> 4h/6h/12h+ widening snap
// table and for an off-grid saved interval snapping back to its nearest step.
func TestLibraryScheduleSliderSnapsToStepsInBrowser(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available, skipping browser-driven slider snapping check")
	}
	if err := exec.Command("node", "-e", "require.resolve('playwright')").Run(); err != nil {
		t.Skip("playwright module not available, skipping browser-driven slider snapping check")
	}

	server := NewServer("", NewBroker())
	server.LibrarySchedules = &localruntime.LibrarySchedules{Path: filepath.Join(t.TempDir(), "config.json"), Init: localruntime.InitConfig{Version: "v1"}}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "tests/library_schedules.cjs")
	cmd.Env = append(os.Environ(), "HARNESS_TEST_URL="+ts.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("library schedule slider snapping check failed:\n%s", out)
	}
}
