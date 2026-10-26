package localruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

func TestLibraryScheduleKeyRejectsDotDotPathSegment(t *testing.T) {
	t.Parallel()
	// "acme/.." matches scheduleRepoPattern's character class but must still
	// be rejected by the explicit "." / ".." segment check.
	if _, err := LibraryScheduleKey("unit-test-coverage", "acme/.."); err == nil {
		t.Fatal("LibraryScheduleKey(repo with .. segment) error = nil, want invalid repository")
	}
}

func TestLibrarySchedulesNowAndPathDefaults(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")

	var s LibrarySchedules
	before := time.Now().UTC()
	got := s.now()
	after := time.Now().UTC()
	if got.Before(before) || got.After(after) {
		t.Fatalf("now() = %v, want between %v and %v", got, before, after)
	}

	if gotPath, want := s.path(), defaultRuntimeConfigPath(); gotPath != want {
		t.Fatalf("path() = %q, want default %q", gotPath, want)
	}
}

func TestSchedulesFromDocMissingKey(t *testing.T) {
	t.Parallel()
	result, err := schedulesFromDoc(map[string]any{})
	if err != nil || len(result) != 0 {
		t.Fatalf("schedulesFromDoc(no key) = %+v, %v, want empty map, nil", result, err)
	}
}

func TestSchedulesFromDocMalformedShape(t *testing.T) {
	t.Parallel()
	_, err := schedulesFromDoc(map[string]any{"library_schedules": "not-a-map"})
	if err == nil || !strings.Contains(err.Error(), "read library schedules") {
		t.Fatalf("schedulesFromDoc(wrong shape) error = %v, want read library schedules failure", err)
	}
}

func TestSchedulesFromDocRejectsInvalidPersistedEntry(t *testing.T) {
	t.Parallel()
	_, err := schedulesFromDoc(map[string]any{
		"library_schedules": map[string]any{
			"unit-test-coverage@acme/repo": map[string]any{
				"libraryTaskName": "unit-test-coverage",
				"repo":            "acme/repo",
				"intervalHours":   0, // invalid: below minimum
				"key":             "unit-test-coverage@acme/repo",
				"nextRunAt":       "2026-01-01T00:00:00Z",
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid persisted library schedule") {
		t.Fatalf("schedulesFromDoc(invalid entry) error = %v, want invalid persisted schedule", err)
	}
}

func TestSchedulesFromDocMarshalError(t *testing.T) {
	t.Parallel()
	_, err := schedulesFromDoc(map[string]any{"library_schedules": make(chan int)})
	if err == nil {
		t.Fatal("schedulesFromDoc(unmarshalable value) error = nil, want marshal failure")
	}
}

func TestLibrarySchedulesTransactionPropagatesLoadError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &LibrarySchedules{Path: path}
	if _, err := s.List(); err == nil {
		t.Fatal("List(malformed config) error = nil, want load failure")
	}
}

func TestLibrarySchedulesTransactionPropagatesInvalidPersistedEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"library_schedules":{"bad-key":{"libraryTaskName":"unit-test-coverage","repo":"acme/repo","intervalHours":1,"key":"bad-key","nextRunAt":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &LibrarySchedules{Path: path}
	if _, err := s.List(); err == nil {
		t.Fatal("List(invalid persisted entries) error = nil, want validation failure")
	}
}

func TestLibrarySchedulesTransactionPropagatesWriteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s := &LibrarySchedules{Path: filepath.Join(dir, "config.json"), Init: InitConfig{Version: "v1"}}
	if _, err := s.Save(scheduleTestInput()); err == nil {
		t.Fatal("Save(unwritable dir) error = nil, want write failure")
	}
}

func TestLibrarySchedulesDeleteRejectsEmptyKey(t *testing.T) {
	t.Parallel()
	s := newScheduleTestStore(t)
	if err := s.Delete("   "); err == nil {
		t.Fatal("Delete(blank key) error = nil, want error")
	}
}

func TestLibrarySchedulesRunDueNoopWithoutSubmit(t *testing.T) {
	t.Parallel()
	s := newScheduleTestStore(t)
	if _, err := s.Save(scheduleTestInput()); err != nil {
		t.Fatal(err)
	}
	// s.Submit is left nil; RunDue must return immediately without error and
	// without attempting to list or reserve any schedule.
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatalf("RunDue(no Submit) error = %v, want nil", err)
	}
}

func TestLibrarySchedulesRunDueStopsOnContextCancelMidLoop(t *testing.T) {
	t.Parallel()
	s := newScheduleTestStore(t)
	input := scheduleTestInput()
	input.IntervalHours = 1
	if _, err := s.Save(input); err != nil {
		t.Fatal(err)
	}
	input.Repo = "acme/second"
	if _, err := s.Save(input); err != nil {
		t.Fatal(err)
	}
	future := s.now().Add(2 * time.Hour)
	s.Now = func() time.Time { return future }

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	s.Submit = func(context.Context, config.Config) (string, error) {
		calls++
		cancel() // cancel after the first due schedule is processed
		return "req", nil
	}
	err := s.RunDue(ctx)
	if err == nil {
		t.Fatal("RunDue(canceled mid-loop) error = nil, want context error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 (loop must stop once ctx is canceled)", calls)
	}
}

func TestLibrarySchedulesRunDueReservationWriteFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := &LibrarySchedules{Path: filepath.Join(dir, "config.json"), Init: InitConfig{Version: "v1"}, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	input := scheduleTestInput()
	input.IntervalHours = 1
	if _, err := s.Save(input); err != nil {
		t.Fatal(err)
	}
	future := s.now().Add(2 * time.Hour)
	s.Now = func() time.Time { return future }
	s.Submit = func(context.Context, config.Config) (string, error) { return "req", nil }

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := s.RunDue(context.Background()); err == nil {
		t.Fatal("RunDue(unwritable dir) error = nil, want reservation write failure")
	}
}

func TestLibrarySchedulesRunDueLogsSubmitErrorAndWriteBackFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := &LibrarySchedules{Path: path, Init: InitConfig{Version: "v1"}, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	input := scheduleTestInput()
	input.IntervalHours = 1
	if _, err := s.Save(input); err != nil {
		t.Fatal(err)
	}
	future := s.now().Add(2 * time.Hour)
	s.Now = func() time.Time { return future }

	var logs []string
	s.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	// Submit fails (exercising the Logf "status=error" branch) and, as a side
	// effect, makes the config directory unwritable so RunDue's trailing
	// write-back (recording LastError) itself fails too - distinct from the
	// earlier reservation write, which must have already succeeded.
	s.Submit = func(context.Context, config.Config) (string, error) {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		return "", context.DeadlineExceeded
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := s.RunDue(context.Background())
	if err == nil {
		t.Fatal("RunDue(write-back failure after submit error) error = nil, want write failure")
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "status=error") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs = %v, want a status=error entry even though the write-back failed afterward", logs)
	}
}
