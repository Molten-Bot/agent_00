package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

func newScheduleTestStore(t *testing.T) *LibrarySchedules {
	t.Helper()
	return &LibrarySchedules{Path: filepath.Join(t.TempDir(), "config.json"), Init: InitConfig{Version: "v1"}, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
}
func scheduleTestInput() LibraryScheduleInput {
	return LibraryScheduleInput{LibraryTaskName: "unit-test-coverage", Repo: "acme/repo", IntervalHours: 24, TargetSubdir: "src", Reviewers: []string{"reviewer"}}
}

func TestLibrarySchedulesIdentityAndPersistence(t *testing.T) {
	t.Parallel()
	s := newScheduleTestStore(t)
	input := scheduleTestInput()
	for _, repo := range []string{"acme/repo", "git@github.com:Acme/Repo.git", "https://github.com/acme/repo/", "ssh://git@github.com/acme/repo.git"} {
		input.Repo = repo
		if _, err := s.Save(input); err != nil {
			t.Fatal(err)
		}
	}
	input.IntervalHours = 6
	saved, err := s.Save(input)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Key != "unit-test-coverage@acme/repo" || saved.NextRunAt != s.now().Add(6*time.Hour) {
		t.Fatalf("unexpected schedule: %+v", saved)
	}
	restarted := &LibrarySchedules{Path: s.Path, Init: s.Init}
	entries, err := restarted.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].IntervalHours != 6 {
		t.Fatalf("duplicate/update lost: %+v", entries)
	}
	if err := IncrementRuntimeConfigLibraryTaskUsage(s.Path, s.Init, "unit-test-coverage"); err != nil {
		t.Fatal(err)
	}
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(s.Path, s.Init, ReviewWatchConfig{ReviewLevel: "low"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err = restarted.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("settings overwrote schedules: %+v %v", entries, err)
	}
	input.Repo = "acme/second"
	if _, err := s.Save(input); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(saved.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(saved.Key); err != nil {
		t.Fatal(err)
	}
	entries, err = restarted.List()
	if err != nil || len(entries) != 1 || entries[0].Repo != "https://github.com/acme/second.git" {
		t.Fatalf("delete: %+v %v", entries, err)
	}
	if ReadRuntimeConfigLibraryTaskUsage(s.Path)["unit-test-coverage"] != 1 {
		t.Fatal("schedule edit overwrote unrelated preferences")
	}
}

func TestLibrarySchedulesDueRestartOverlapAndFailure(t *testing.T) {
	t.Parallel()
	s := newScheduleTestStore(t)
	now := s.now()
	s.Now = func() time.Time { return now }
	input := scheduleTestInput()
	input.IntervalHours = 1
	saved, err := s.Save(input)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	active := false
	var submitErr error
	s.Active = func(LibrarySchedule) bool { return active }
	s.Submit = func(_ context.Context, cfg config.Config) (string, error) {
		calls++
		if cfg.LibraryTaskName != input.LibraryTaskName || cfg.TargetSubdir != "src" || cfg.Prompt == "" || len(cfg.Reviewers) != 1 {
			t.Fatalf("bad expanded run: %+v", cfg)
		}
		return fmt.Sprint(calls), submitErr
	}
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("ran immediately on save")
	}
	now = now.Add(3 * time.Hour)
	active = true
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("overlapping run")
	}
	active = false
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("missed runs replayed: %d", calls)
	}
	s = &LibrarySchedules{Path: s.Path, Init: s.Init, Now: s.Now, Submit: s.Submit}
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("restart replayed occurrence")
	}
	now = now.Add(time.Hour)
	submitErr = errors.New("agent authorization required")
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].LastError == "" || !entries[0].NextRunAt.After(now) {
		t.Fatal("failure not visible or next attempt not persisted")
	}
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("failure retried every tick")
	}
	if err := s.Delete(saved.Key); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("deleted schedule ran")
	}
}

// Run is a thin wrapper that loops RunDue on a 30-second ticker until its
// context is canceled. RunDue itself already has thorough coverage above via
// injected Now/Active/Submit, so these tests focus on Run's own control flow:
// it must perform an initial RunDue pass without waiting for the ticker, it
// must propagate RunDue failures to Logf, and it must return promptly on
// context cancellation rather than blocking for the full ticker interval.
func TestLibrarySchedulesRunPerformsInitialPassAndStopsOnCancel(t *testing.T) {
	t.Parallel()

	// A thread-safe clock: the test goroutine advances it past the schedule's
	// due time while Run's background goroutine reads it concurrently.
	var nowNano atomic.Int64
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowNano.Store(base.UnixNano())

	s := &LibrarySchedules{
		Path: filepath.Join(t.TempDir(), "config.json"),
		Init: InitConfig{Version: "v1"},
		Now:  func() time.Time { return time.Unix(0, nowNano.Load()).UTC() },
	}
	input := scheduleTestInput()
	input.IntervalHours = 1
	if _, err := s.Save(input); err != nil {
		t.Fatal(err)
	}
	// The schedule isn't due until base+1h; advance the clock so Run's first
	// pass finds it due immediately instead of waiting on the 30s ticker.
	nowNano.Store(base.Add(2 * time.Hour).UnixNano())

	var mu sync.Mutex
	submitCalls := 0
	s.Submit = func(context.Context, config.Config) (string, error) {
		mu.Lock()
		submitCalls++
		mu.Unlock()
		return "req-1", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		calls := submitCalls
		mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run() never performed its initial RunDue pass")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after context cancellation")
	}
}

func TestLibrarySchedulesRunLogsRunDueErrors(t *testing.T) {
	t.Parallel()

	// Pointing Path at a directory makes every RunDue call fail with a
	// non-not-exist read error, exercising Run's error-logging branch. Submit
	// must be non-nil for RunDue to attempt List() at all.
	s := &LibrarySchedules{
		Path:   t.TempDir(),
		Init:   InitConfig{Version: "v1"},
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		Submit: func(context.Context, config.Config) (string, error) { return "", nil },
	}

	var mu sync.Mutex
	var logs []string
	s.Logf = func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(logs)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run() never logged a RunDue error")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after context cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 || !strings.Contains(logs[0], "status=error") {
		t.Fatalf("logs = %v, want a library.schedule status=error entry", logs)
	}
}

func TestLibraryScheduleInputRunConfigIncludesDraft(t *testing.T) {
	t.Parallel()
	input := scheduleTestInput()
	input.Draft = true
	cfg, err := input.RunConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Draft {
		t.Fatalf("expected draft to carry into run config: %+v", cfg)
	}
}

func TestLibrarySchedulesPersistDraft(t *testing.T) {
	t.Parallel()
	s := newScheduleTestStore(t)
	input := scheduleTestInput()
	input.Draft = true
	saved, err := s.Save(input)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Draft {
		t.Fatalf("expected saved schedule to keep draft=true: %+v", saved)
	}
	restarted := &LibrarySchedules{Path: s.Path, Init: s.Init}
	entries, err := restarted.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Draft {
		t.Fatalf("draft not persisted: %+v", entries)
	}
	s.Submit = func(_ context.Context, cfg config.Config) (string, error) {
		if !cfg.Draft {
			t.Fatalf("expected scheduled run to submit as draft: %+v", cfg)
		}
		return "req-1", nil
	}
	now := s.now().Add(25 * time.Hour)
	s.Now = func() time.Time { return now }
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryScheduleInputRunConfigCarriesPromptContext(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HARNESS_LIBRARY_DIR", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "task.json"), []byte(`{
  "name": "needs-context",
  "description": "Needs context.",
  "prompt": "Do the thing.",
  "promptContextLabel": "Details",
  "promptContextRequired": true
}`), 0o644); err != nil {
		t.Fatalf("write library task: %v", err)
	}

	input := scheduleTestInput()
	input.LibraryTaskName = "needs-context"

	if _, err := input.RunConfig(); err == nil {
		t.Fatal("expected error when promptContext is required but missing")
	}

	input.PromptContext = "Repro steps here."
	cfg, err := input.RunConfig()
	if err != nil {
		t.Fatalf("RunConfig() error = %v", err)
	}
	if !strings.Contains(cfg.Prompt, "Repro steps here.") {
		t.Fatalf("cfg.Prompt = %q, want it to include promptContext", cfg.Prompt)
	}
}

func TestLibrarySchedulesRunDueSubmitsPromptContext(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HARNESS_LIBRARY_DIR", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "task.json"), []byte(`{
  "name": "needs-context",
  "description": "Needs context.",
  "prompt": "Do the thing.",
  "promptContextLabel": "Details",
  "promptContextRequired": true
}`), 0o644); err != nil {
		t.Fatalf("write library task: %v", err)
	}

	s := newScheduleTestStore(t)
	input := scheduleTestInput()
	input.LibraryTaskName = "needs-context"
	input.PromptContext = "Repro steps here."
	if _, err := s.Save(input); err != nil {
		t.Fatal(err)
	}

	var submitted config.Config
	calls := 0
	s.Submit = func(_ context.Context, cfg config.Config) (string, error) {
		calls++
		submitted = cfg
		return "req-1", nil
	}
	now := s.now().Add(25 * time.Hour)
	s.Now = func() time.Time { return now }
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (promptContextRequired task should be runnable, not error every occurrence)", calls)
	}
	if !strings.Contains(submitted.Prompt, "Repro steps here.") {
		t.Fatalf("submitted.Prompt = %q, want it to include promptContext", submitted.Prompt)
	}
}

func TestLibrarySchedulesValidation(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*LibraryScheduleInput){
		func(i *LibraryScheduleInput) { i.IntervalHours = 0 }, func(i *LibraryScheduleInput) { i.IntervalHours = MaxLibraryScheduleHours + 1 },
		func(i *LibraryScheduleInput) { i.LibraryTaskName = "missing-task" }, func(i *LibraryScheduleInput) { i.LibraryTaskName = "../task" },
		func(i *LibraryScheduleInput) { i.Repo = "https://example.com/acme/repo" }, func(i *LibraryScheduleInput) { i.Repo = "https://token@github.com/acme/repo" },
		func(i *LibraryScheduleInput) { i.Repo = "acme/repo/extra" }, func(i *LibraryScheduleInput) { i.Repo = "https://github.com/acme/repo?token=x" },
	} {
		s := newScheduleTestStore(t)
		input := scheduleTestInput()
		change(&input)
		if _, err := s.Save(input); err == nil {
			t.Fatalf("accepted invalid input: %+v", input)
		}
		if _, err := os.Stat(s.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid schedule persisted")
		}
	}
}

func TestLibraryScheduleConcurrentSettingsWrites(t *testing.T) {
	t.Parallel()
	s := newScheduleTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			input := scheduleTestInput()
			input.Repo = fmt.Sprintf("acme/repo-%d", i)
			if _, err := s.Save(input); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if err := IncrementRuntimeConfigLibraryTaskUsage(s.Path, s.Init, "unit-test-coverage"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	entries, err := s.List()
	if err != nil || len(entries) != 12 {
		t.Fatalf("lost schedules: count=%d err=%v", len(entries), err)
	}
	if n := ReadRuntimeConfigLibraryTaskUsage(s.Path)["unit-test-coverage"]; n != 12 {
		t.Fatalf("lost preference updates: %d", n)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
}
