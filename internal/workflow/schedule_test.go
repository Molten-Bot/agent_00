package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func scheduledDefinition() Definition {
	w := definition("scheduled")
	w.Trigger = Trigger{Event: "schedule", IntervalHours: 24}
	return w
}
func TestScheduleMinuteAndMonthIntervals(t *testing.T) {
	t.Parallel()
	for _, minutes := range []int{1, 7, 30 * 24 * 60, 12 * 30 * 24 * 60} {
		t.Run((time.Duration(minutes) * time.Minute).String(), func(t *testing.T) {
			w := scheduledDefinition()
			w.Trigger.IntervalHours = float64(minutes) / 60
			e, _, count := testEngine(t, w)
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			e.Now = func() time.Time { return now }
			tick(t, e)
			status, err := e.ScheduleStatus()
			want := now.Add(time.Duration(minutes) * time.Minute)
			if err != nil || len(status) != 1 || !status[0].NextRunAt.Equal(want) {
				t.Fatalf("next run = %+v, %v; want %v", status, err, want)
			}
			now = want.Add(-time.Second)
			tick(t, e)
			if *count != 0 {
				t.Fatal("ran before interval elapsed")
			}
			now = want
			tick(t, e)
			if *count != 1 {
				t.Fatal("did not run when interval elapsed")
			}
			status, err = e.ScheduleStatus()
			if err != nil || !status[0].NextRunAt.Equal(now.Add(time.Duration(minutes)*time.Minute)) {
				t.Fatalf("next occurrence = %+v, %v", status, err)
			}
		})
	}
}
func TestScheduleDueRestartCoalescingAndNoOverlap(t *testing.T) {
	t.Parallel()
	w := scheduledDefinition()
	e, _, count := testEngine(t, w)
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	e.Fetch = func(context.Context, string, ScanNeeds) ([]Item, error) {
		t.Fatal("scheduled task fetched GitHub events")
		return nil, nil
	}
	tick(t, e)
	if *count != 0 {
		t.Fatal("ran before due")
	}
	status, err := e.ScheduleStatus()
	if err != nil || !status[0].NextRunAt.Equal(now.Add(24*time.Hour)) {
		t.Fatal(status, err)
	}
	now = now.Add(24 * time.Hour)
	tick(t, e)
	if *count != 1 {
		t.Fatal("due schedule did not run")
	}
	runs, _ := e.Runs()
	if runs[0].Item.Kind != "schedule" {
		t.Fatal(runs)
	}
	restarted := &Engine{Store: e.Store, StatePath: e.StatePath, Now: e.Now, Enabled: e.Enabled, Allowed: e.Allowed, Fetch: e.Fetch, Submit: e.Submit, Status: e.Status}
	now = now.Add(7 * 24 * time.Hour)
	tick(t, restarted)
	if *count != 1 {
		t.Fatal("overlapped running occurrence")
	}
	if err := restarted.Complete(runs[0].ID, Outcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	tick(t, restarted)
	tick(t, restarted)
	if *count != 2 {
		t.Fatal("downtime was replayed or lost", *count)
	}
	status, _ = restarted.ScheduleStatus()
	if !status[0].NextRunAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatal(status)
	}
}
func TestScheduleGatesAndSubmissionFailureAdvanceOnce(t *testing.T) {
	t.Parallel()
	w := scheduledDefinition()
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	w.Trigger.StartAt = &now
	e, _, _ := testEngine(t, w)
	e.Now = func() time.Time { return now }
	calls := 0
	e.Submit = func(context.Context, Run) (string, error) { calls++; return "", errors.New("auth unavailable") }
	e.Enabled = func() bool { return false }
	tick(t, e)
	e.Enabled = func() bool { return true }
	e.Allowed = func(string) bool { return false }
	tick(t, e)
	e.Allowed = func(string) bool { return true }
	e.Active = func(Definition) bool { return true }
	tick(t, e)
	if calls != 0 {
		t.Fatal("gate bypassed")
	}
	e.Active = func(Definition) bool { return false }
	tick(t, e)
	tick(t, e)
	if calls != 1 {
		t.Fatal("failure loop", calls)
	}
	runs, _ := e.Runs()
	if len(runs) != 0 {
		t.Fatal(runs)
	}
	now = now.Add(24 * time.Hour)
	tick(t, e)
	if calls != 2 {
		t.Fatal("did not retry next interval")
	}
}
func TestScheduleChangesAndDefinitionValidation(t *testing.T) {
	t.Parallel()
	w := scheduledDefinition()
	e, _, count := testEngine(t, w)
	now := time.Now().UTC()
	e.Now = func() time.Time { return now }
	tick(t, e)
	doc, _ := e.Store.Load()
	doc.Workflows[0].Name = "Renamed"
	doc.Workflows[0].Task.Instructions = "Changed prompt"
	e.Store.Save(doc)
	now = now.Add(2 * time.Hour)
	tick(t, e)
	status, _ := e.ScheduleStatus()
	if !status[0].NextRunAt.Equal(now.Add(22 * time.Hour)) {
		t.Fatal("prompt edit reset schedule")
	}
	doc, _ = e.Store.Load()
	doc.Workflows[0].Trigger.IntervalHours = 6
	e.Store.Save(doc)
	tick(t, e)
	status, _ = e.ScheduleStatus()
	if !status[0].NextRunAt.Equal(now.Add(6*time.Hour)) || *count != 0 {
		t.Fatal(status)
	}
	for _, hours := range []float64{0, -1, 8761, 0.01, 1.001} {
		w.Trigger.IntervalHours = hours
		if w.Validate() == nil {
			t.Fatal("accepted interval", hours)
		}
	}
	w = scheduledDefinition()
	w.Filters.Rules = []Rule{{Field: "label", Operator: "is", Value: "ready"}}
	if w.Validate() == nil {
		t.Fatal("accepted GitHub filters on timer")
	}
	w = scheduledDefinition()
	w.Task.Instructions = ""
	w.Task.LibraryTaskName = "unit-test-coverage"
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestV1DefinitionsUpgradeWithOriginalBackup(t *testing.T) {
	t.Parallel()
	e, _, _ := testEngine(t, definition("one"))
	doc, _ := e.Store.Load()
	doc.Version = 1
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(e.Store.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	upgraded, err := e.Store.Load()
	if err != nil || upgraded.Version != Version {
		t.Fatal(upgraded, err)
	}
	if _, err = e.Store.Save(upgraded); err != nil {
		t.Fatal(err)
	}
	backup, _ := os.ReadFile(e.Store.Path + ".v1.bak")
	if string(backup) != string(raw) {
		t.Fatal("original v1 definition not retained")
	}
	round, _ := e.Store.Load()
	if round.Workflows[0].ID != "one" {
		t.Fatal("identity changed")
	}
}
func TestScheduledPreviewDoesNotCreateCursor(t *testing.T) {
	t.Parallel()
	w := scheduledDefinition()
	e, _, count := testEngine(t, w)
	decisions, err := e.Preview(context.Background(), w)
	if err != nil || len(decisions) != 1 || decisions[0].Item.Kind != "schedule" || *count != 0 {
		t.Fatal(decisions, err)
	}
	if _, err = os.Stat(e.StatePath); !os.IsNotExist(err) {
		t.Fatal("preview wrote state")
	}
}

func TestScheduledRepositoryIdentityMatchesLegacyURLForms(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"acme/repo", "git@github.com:acme/repo.git", "https://github.com/Acme/Repo.git", "ssh://git@github.com/acme/repo.git", "github.com/acme/repo"} {
		if got := CanonicalRepository(value); got != "acme/repo" {
			t.Fatalf("%s: %s", value, got)
		}
	}
}

func TestEmptyReviewerOverrideSurvivesDefinitionRoundTrip(t *testing.T) {
	t.Parallel()
	w := scheduledDefinition()
	empty := []string{}
	w.Task.Reviewers = &empty
	raw, err := json.Marshal(Document{Version: Version, Workflows: []Definition{w}})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Decode(raw)
	if err != nil || doc.Workflows[0].Task.Reviewers == nil || len(*doc.Workflows[0].Task.Reviewers) != 0 {
		t.Fatal("empty override lost", err)
	}
}
