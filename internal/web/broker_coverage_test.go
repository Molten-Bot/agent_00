package web

import (
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/library"
)

func TestLibraryTaskDisplayText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		task library.TaskSummary
		want string
	}{
		{
			name: "title and description combine",
			task: library.TaskSummary{DisplayName: " Ship It ", Description: " ship the release "},
			want: "Ship It\n\nship the release",
		},
		{
			name: "title falls back to name when display name is blank",
			task: library.TaskSummary{Name: "ship-it"},
			want: "ship-it",
		},
		{
			name: "description only",
			task: library.TaskSummary{Description: " description text "},
			want: "description text",
		},
		{
			name: "falls back to prompt when nothing else is set",
			task: library.TaskSummary{Prompt: " do the thing "},
			want: "do the thing",
		},
		{
			name: "entirely empty task yields empty text",
			task: library.TaskSummary{},
			want: "",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := libraryTaskDisplayText(tt.task); got != tt.want {
				t.Fatalf("libraryTaskDisplayText(%#v) = %q, want %q", tt.task, got, tt.want)
			}
		})
	}
}

func TestIsClosedTaskLockedTracksRetentionWindow(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.isClosedTaskLocked("missing-task", now) {
		t.Fatal("isClosedTaskLocked(unknown request) = true, want false")
	}

	b.closedTasks["req-recent"] = now.Add(-time.Hour)
	b.runConfigs["req-recent"] = []byte(`{}`)
	if !b.isClosedTaskLocked("req-recent", now) {
		t.Fatal("isClosedTaskLocked(recently closed) = false, want true")
	}

	b.closedTasks["req-expired"] = now.Add(-25 * time.Hour)
	b.runConfigs["req-expired"] = []byte(`{}`)
	if b.isClosedTaskLocked("req-expired", now) {
		t.Fatal("isClosedTaskLocked(expired retention) = true, want false")
	}
	if _, ok := b.closedTasks["req-expired"]; ok {
		t.Fatal("isClosedTaskLocked(expired) did not evict closedTasks entry")
	}
	if _, ok := b.runConfigs["req-expired"]; ok {
		t.Fatal("isClosedTaskLocked(expired) did not evict runConfigs entry")
	}
}

func TestRecordReleaseFromTaskEdgeCases(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	b := NewBroker()
	b.now = func() time.Time { return now }

	// Missing request id or PR URL must be a no-op.
	b.RecordReleaseFromTask(Task{}, "")
	b.RecordReleaseFromTask(Task{RequestID: "req-1"}, "")
	b.RecordReleaseFromTask(Task{PRURL: "https://github.com/acme/repo/pull/1"}, "")
	if got := len(b.Snapshot().Releases); got != 0 {
		t.Fatalf("releases after no-op calls = %d, want 0", got)
	}

	start := now.Add(-10 * time.Minute)
	completed := now.Add(-5 * time.Minute)
	b.RecordReleaseFromTask(Task{
		RequestID: "req-1",
		PRURL:     "https://github.com/acme/repo/pull/1",
		StartedAt: start.Format(time.RFC3339),
		UpdatedAt: completed.Format(time.RFC3339),
	}, "")

	releases := b.Snapshot().Releases
	if len(releases) != 1 {
		t.Fatalf("releases after first record = %d, want 1", len(releases))
	}
	if releases[0].DurationSeconds != (5 * time.Minute).Seconds() {
		t.Fatalf("release duration seconds = %f, want %f (fallback from started/completed)", releases[0].DurationSeconds, (5 * time.Minute).Seconds())
	}
	if releases[0].MergedAt != now.Format(time.RFC3339) {
		t.Fatalf("release merged at = %q, want fallback to broker clock %q", releases[0].MergedAt, now.Format(time.RFC3339))
	}

	// Recording the same request id again updates the entry in place.
	b.RecordReleaseFromTask(Task{
		RequestID:       "req-1",
		PRURL:           "https://github.com/acme/repo/pull/1",
		DurationSeconds: 42,
	}, now.Format(time.RFC3339))
	releases = b.Snapshot().Releases
	if len(releases) != 1 {
		t.Fatalf("releases after update = %d, want 1 (update in place rather than append)", len(releases))
	}
	if releases[0].DurationSeconds != 42 {
		t.Fatalf("updated release duration seconds = %f, want 42", releases[0].DurationSeconds)
	}
}
