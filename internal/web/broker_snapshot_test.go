package web

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// compactStreamSnapshot is the reference compaction that StreamSnapshot must
// match: drop events and cap each task's logs to the stream tail.
func compactStreamSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Events = nil
	for i := range snapshot.Tasks {
		logs := snapshot.Tasks[i].Logs
		if len(logs) <= maxStreamTaskLogs {
			continue
		}
		snapshot.Tasks[i].Logs = compactStreamTaskLogs(logs)
	}
	return snapshot
}

func newSnapshotTestBroker(t *testing.T) *Broker {
	t.Helper()
	b := NewBroker()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	b.now = func() time.Time { return now }
	b.RecordTaskRunConfig("req-long", []byte(`{"repo":"https://github.com/acme/repo","prompt":"long task","agentHarness":"claude","reviewers":["@octo"],"baseBranch":"main","prUrl":"https://github.com/acme/repo/pull/7"}`))
	b.IngestLog("dispatch request_id=req-long stage=clone status=ok")
	for i := 0; i < maxStreamTaskLogs+25; i++ {
		b.IngestLog(fmt.Sprintf("dispatch request_id=req-long stage=agent status=running line=%d", i))
	}
	b.IngestLog("dispatch request_id=req-short stage=clone status=start")
	return b
}

func TestStreamSnapshotMatchesCompactedSnapshot(t *testing.T) {
	t.Parallel()

	b := newSnapshotTestBroker(t)
	full := b.Snapshot()
	want := compactStreamSnapshot(full)
	got := b.StreamSnapshot()
	want.GeneratedAt = got.GeneratedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StreamSnapshot() differs from compactStreamSnapshot(Snapshot())\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestSnapshotTimestampsAreStrictlyIncreasing(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.now = func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}

	first := b.Snapshot()
	second := b.StreamSnapshot()
	firstGeneratedAt, err := time.Parse(time.RFC3339Nano, first.GeneratedAt)
	if err != nil {
		t.Fatalf("parse first generated_at: %v", err)
	}
	secondGeneratedAt, err := time.Parse(time.RFC3339Nano, second.GeneratedAt)
	if err != nil {
		t.Fatalf("parse second generated_at: %v", err)
	}
	if !secondGeneratedAt.After(firstGeneratedAt) {
		t.Fatalf("second generated_at = %q, want after %q", second.GeneratedAt, first.GeneratedAt)
	}
}

func TestSnapshotTimestampGeneratedWhileStateLocked(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	clockCalledOutsideLock := false
	b.now = func() time.Time {
		if b.mu.TryLock() {
			clockCalledOutsideLock = true
			b.mu.Unlock()
		}
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}

	b.Snapshot()
	if clockCalledOutsideLock {
		t.Fatal("Snapshot() generated timestamp before locking state")
	}
}

func TestTaskListMatchesSnapshotTasksWithoutLogs(t *testing.T) {
	t.Parallel()

	b := newSnapshotTestBroker(t)
	want := b.Snapshot().Tasks
	for i := range want {
		want[i].Logs = nil
	}
	if got := b.TaskList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TaskList() = %+v, want %+v", got, want)
	}
}

func TestTaskListTracksPeakConcurrency(t *testing.T) {
	t.Parallel()

	b := newSnapshotTestBroker(t)
	_ = b.TaskList()
	if b.maxConcurrentTasks == 0 {
		t.Fatal("TaskList() did not record peak concurrency for active tasks")
	}
}

func TestRunConfigFieldCacheRefreshesWhenRunConfigChanges(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.RecordTaskRunConfig("req-1", []byte(`{"repo":"https://github.com/acme/repo","prompt":"p","agentHarness":"codex"}`))
	b.mu.Lock()
	first := b.runConfigFieldsLocked("req-1")
	b.mu.Unlock()
	if first.agentHarness != "codex" {
		t.Fatalf("agentHarness = %q, want codex", first.agentHarness)
	}

	b.RecordTaskRunConfig("req-1", []byte(`{"repo":"https://github.com/acme/repo","prompt":"p","agentHarness":"claude"}`))
	b.mu.Lock()
	second := b.runConfigFieldsLocked("req-1")
	b.mu.Unlock()
	if second.agentHarness != "claude" {
		t.Fatalf("agentHarness after update = %q, want claude", second.agentHarness)
	}
}
