package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
