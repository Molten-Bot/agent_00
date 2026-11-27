package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTaskDeleteDismissesImmediatelyAndRetriesInBrowser(t *testing.T) {
	requireBrowserTooling(t, "task deletion checks")

	b := NewBroker()
	b.IngestLog("task status=completed request_id=earlier")
	b.RecordTaskRerunAttempt("earlier", "finished")
	b.IngestLog("task status=start request_id=finished")
	b.IngestLog("task status=completed request_id=finished")
	b.IngestLog("task status=start request_id=other")
	b.IngestLog("task status=completed request_id=other")
	b.IngestLog("task status=start request_id=running")
	b.IngestLog("task status=error request_id=resumed exit_code=1")
	b.IngestLog("task status=completed request_id=handoff")
	srv := NewServer("", b)
	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
	mux.HandleFunc("POST /test/rerun", func(w http.ResponseWriter, r *http.Request) {
		b.RecordTaskRerunAttempt("finished", "later")
		b.IngestLog("task status=start request_id=later")
		b.IngestLog("task status=completed request_id=later")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /test/resume", func(w http.ResponseWriter, r *http.Request) {
		b.IngestLog("task status=queued request_id=resumed action=resume_draft_verification")
		b.IngestLog("task status=completed request_id=resumed")
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	runBrowserSuite(t, ts.URL, time.Minute, "task deletion checks", "tests/task_delete.cjs")
	if snap := b.Snapshot(); len(snap.Tasks) != 2 || snap.Stats.CompletedTasks != 1 || snap.Stats.ActiveTasks != 1 || snap.Stats.TotalTasks != 2 {
		t.Fatalf("expected the newer rerun and running task after retry: %#v", snap)
	}
	if attempts := b.TaskAttempts("later"); len(attempts) != 1 || attempts[0].RequestID != "later" {
		t.Fatalf("expected only the newer rerun in the attempt chain: %#v", attempts)
	}
}

func TestTaskDeleteRemovesCompletedRerunChain(t *testing.T) {
	t.Parallel()
	for _, closed := range []bool{false, true} {
		b := NewBroker()
		b.IngestLog("task status=start request_id=original")
		b.IngestLog(`task status=error request_id=original exit_code=1 err="failed"`)
		b.RecordTaskRerunAttempt("original", "rerun")
		b.IngestLog("task status=start request_id=rerun")
		if got := b.Snapshot().Stats; got.TotalTasks != 1 || got.FailedTasks != 0 || got.ActiveTasks != 1 {
			t.Fatalf("running rerun: %#v", got)
		}
		b.IngestLog("task status=completed request_id=rerun")
		if closed {
			if err := b.CloseTask("rerun"); err != nil {
				t.Fatal(err)
			}
		}
		srv := NewServer("", b)
		resp := httptest.NewRecorder()
		srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/tasks/rerun/delete", nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("delete = %d: %s", resp.Code, resp.Body.String())
		}
		for range 2 {
			snap := b.Snapshot()
			if snap.Stats.TotalTasks != 0 || snap.Stats.CompletedTasks != 0 || snap.Stats.FailedTasks != 0 || len(snap.Tasks) != 0 {
				t.Fatalf("deleted chain resurfaces: %#v", snap)
			}
		}
	}
}

func TestTaskDeleteRejectsActiveAndMissingTasks(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	b.IngestLog("task status=start request_id=active")
	srv := NewServer("", b)
	for _, tc := range []struct {
		id   string
		code int
	}{{"active", http.StatusConflict}, {"missing", http.StatusNotFound}} {
		resp := httptest.NewRecorder()
		srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/tasks/"+tc.id+"/delete", nil))
		if resp.Code != tc.code {
			t.Fatalf("delete %s = %d, want %d", tc.id, resp.Code, tc.code)
		}
	}
	if b.Snapshot().Stats.ActiveTasks != 1 {
		t.Fatal("active task was removed")
	}
}

func TestQueuedTaskDeletePreservesResumedGeneration(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.IngestLog("task status=error request_id=original exit_code=1")
	b.RecordTaskRerunAttempt("original", "finished")
	b.IngestLog("task status=completed request_id=finished")
	scope := b.completedAttemptGenerations("finished")
	if scope["original"] == "" || scope["finished"] == "" {
		t.Fatalf("expected generation for both completed attempts: %#v", scope)
	}

	srv := NewServer("", b)
	var cleaned []string
	srv.CloseTask = func(_ context.Context, id string) error {
		cleaned = append(cleaned, id)
		return nil
	}
	handler := srv.Handler()
	postDelete := func(id, body string, want int) {
		t.Helper()
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/tasks/"+id+"/delete", strings.NewReader(body)))
		if resp.Code != want {
			t.Fatalf("delete %s = %d, want %d: %s", id, resp.Code, want, resp.Body.String())
		}
	}
	body, err := json.Marshal(map[string]any{"attempt_generations": scope})
	if err != nil {
		t.Fatal(err)
	}
	b.IngestLog("task status=queued request_id=original action=resume_draft_verification")
	postDelete("original", string(body), http.StatusNotFound)
	b.IngestLog("task status=completed request_id=original")

	// A retry can also reach the broker after the resumed result moved into history.
	if err := b.CloseTask("original"); err != nil {
		t.Fatal(err)
	}
	postDelete("original", string(body), http.StatusNotFound)
	postDelete("original", `{"attempt_ids":["original","finished"]}`, http.StatusNotFound)
	if len(cleaned) != 0 {
		t.Fatalf("stale deletes triggered cleanup: %v", cleaned)
	}

	// Closing the unchanged sibling may remove it, but must keep the resumed result.
	postDelete("finished", string(body), http.StatusOK)
	snap := b.Snapshot()
	if snap.Stats.CompletedTasks != 1 || snap.Stats.TotalTasks != 1 {
		t.Fatalf("expected resumed result in metrics: %+v", snap.Stats)
	}
	if got := b.completedAttemptGenerations("original"); len(got) != 1 || got["original"] == scope["original"] || got["original"] == "" {
		t.Fatalf("expected only resumed attempt, got %v from original scope %v", got, scope)
	}
	if len(cleaned) != 1 || cleaned[0] != "finished" {
		t.Fatalf("expected cleanup only for finished sibling: %v", cleaned)
	}
}

func TestScopedTaskDeleteFinishesChainAfterTargetWasDeleted(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	b.IngestLog("task status=completed request_id=original")
	b.RecordTaskRerunAttempt("original", "finished")
	b.IngestLog("task status=completed request_id=finished")
	scope := b.completedAttemptGenerations("finished")

	srv := NewServer("", b)
	var cleaned []string
	srv.CloseTask = func(_ context.Context, id string) error {
		cleaned = append(cleaned, id)
		return nil
	}
	handler := srv.Handler()
	postDelete := func(generations map[string]string, want int) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"attempt_generations": generations})
		if err != nil {
			t.Fatal(err)
		}
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/tasks/finished/delete", strings.NewReader(string(body))))
		if resp.Code != want {
			t.Fatalf("delete = %d, want %d: %s", resp.Code, want, resp.Body.String())
		}
	}

	postDelete(map[string]string{"finished": scope["finished"]}, http.StatusOK)
	now = now.Add(defaultClosedTaskRetention + time.Second)
	b.IngestLog("task status=completed request_id=unrelated")
	scope["unrelated"] = b.completedAttemptGenerations("unrelated")["unrelated"]
	postDelete(map[string]string{"finished": "unknown-generation", "original": scope["original"]}, http.StatusNotFound)
	postDelete(scope, http.StatusOK)
	postDelete(scope, http.StatusNotFound)

	if len(cleaned) != 1 || cleaned[0] != "finished" {
		t.Fatalf("cleanup ran more than once for the target: %v", cleaned)
	}
	if snap := b.Snapshot(); len(snap.Tasks) != 1 || snap.Tasks[0].RequestID != "unrelated" || snap.Stats.CompletedTasks != 1 {
		t.Fatalf("expected only the unrelated task after scoped retry: %#v", snap)
	}
}

func TestRerunMetricsIgnoreLateOriginalUpdates(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	b.IngestLog("task status=start request_id=original")
	b.IngestLog(`task status=error request_id=original exit_code=1 err="failed"`)
	b.RecordTaskRerunAttempt("original", "rerun")
	b.IngestLog("task status=completed request_id=rerun")
	now = now.Add(time.Second)
	b.IngestLog(`task status=error request_id=original exit_code=1 err="late failure"`)
	got := b.Snapshot().Stats
	if got.TotalTasks != 1 || got.CompletedTasks != 1 || got.FailedTasks != 0 {
		t.Fatalf("late original update replaced rerun: %#v", got)
	}
}
