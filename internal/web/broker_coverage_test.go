package web

import (
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/library"
)

func TestAddTimeStatsGroupAggregatesTasksByNormalizedName(t *testing.T) {
	t.Parallel()

	// A nil groups map must be a safe no-op.
	addTimeStatsGroup(nil, "workflow", "completed", time.Minute)

	groups := map[string]*TimeStatsGroup{}
	addTimeStatsGroup(groups, "  ", "running", 2*time.Minute)
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	group, ok := groups["Unknown"]
	if !ok {
		t.Fatalf("groups = %#v, want fallback name %q", groups, "Unknown")
	}
	if group.Tasks != 1 || group.ActiveTasks != 1 || group.CompletedTasks != 0 {
		t.Fatalf("group after active task = %#v", group)
	}
	if group.AverageDurationSeconds != 0 {
		t.Fatalf("average duration with zero completed tasks = %f, want 0", group.AverageDurationSeconds)
	}
	if group.TotalSavedSeconds != 0 {
		t.Fatalf("saved duration for an active task = %f, want 0", group.TotalSavedSeconds)
	}

	addTimeStatsGroup(groups, "", "completed", 4*time.Minute)
	group = groups["Unknown"]
	if group.Tasks != 2 || group.ActiveTasks != 1 || group.CompletedTasks != 1 {
		t.Fatalf("group after completed task = %#v", group)
	}
	wantTotal := (2 * time.Minute).Seconds() + (4 * time.Minute).Seconds()
	if group.TotalDurationSeconds != wantTotal {
		t.Fatalf("total duration = %f, want %f", group.TotalDurationSeconds, wantTotal)
	}
	if group.TotalSavedSeconds != (4 * time.Minute).Seconds() {
		t.Fatalf("total saved duration = %f, want %f (only successful tasks save time)", group.TotalSavedSeconds, (4 * time.Minute).Seconds())
	}
	wantAverage := wantTotal / float64(group.Tasks)
	if group.AverageDurationSeconds != wantAverage {
		t.Fatalf("average duration = %f, want %f", group.AverageDurationSeconds, wantAverage)
	}

	addTimeStatsGroup(groups, "Release Train", "error", time.Minute)
	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2", len(groups))
	}
	errGroup := groups["Release Train"]
	if errGroup.Tasks != 1 || errGroup.ActiveTasks != 0 || errGroup.CompletedTasks != 0 {
		t.Fatalf("terminal-but-failed status group = %#v, want non-active non-completed", errGroup)
	}
	if errGroup.TotalSavedSeconds != 0 {
		t.Fatalf("failed task saved duration = %f, want 0", errGroup.TotalSavedSeconds)
	}
}

func TestParseFieldValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		key  string
		want string
	}{
		{name: "blank key returns empty", line: `status=start request_id=abc`, key: "   ", want: ""},
		{name: "line without kv fields returns empty", line: "plain log line without fields", key: "status", want: ""},
		{name: "key not present returns empty", line: `status=start request_id=abc`, key: "branch", want: ""},
		{name: "key found returns trimmed value", line: `status=start request_id=abc`, key: "request_id", want: "abc"},
		{name: "quoted value is unquoted", line: `status=start msg="hello world"`, key: "msg", want: "hello world"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseFieldValue(tt.line, tt.key); got != tt.want {
				t.Fatalf("parseFieldValue(%q, %q) = %q, want %q", tt.line, tt.key, got, tt.want)
			}
		})
	}
}

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
