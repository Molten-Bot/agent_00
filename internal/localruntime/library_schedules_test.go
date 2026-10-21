package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	if err := SaveRuntimeConfigReviewSettings(s.Path, s.Init, ReviewWatchConfig{ReviewLevel: "low"}); err != nil {
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

func TestLibrarySchedulesValidation(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*LibraryScheduleInput){
		func(i *LibraryScheduleInput) { i.IntervalHours = 0 }, func(i *LibraryScheduleInput) { i.IntervalHours = 169 },
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
