package workflow

import (
	"context"
	"math"
	"strings"
	"time"
)

type ScheduleCursor struct {
	Policy    string    `json:"policy"`
	NextRunAt time.Time `json:"nextRunAt"`
}
type ScheduleStatus struct {
	WorkflowID string    `json:"workflowId"`
	NextRunAt  time.Time `json:"nextRunAt"`
	Enabled    bool      `json:"enabled"`
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}
func schedulePolicy(w Definition) string {
	return digest([]any{strings.ToLower(w.Repository), w.Trigger})
}
func (e *Engine) scheduleCursor(w Definition, s State) ScheduleCursor {
	cursor := s.Schedules[w.ID]
	policy := schedulePolicy(w)
	if cursor.Policy != policy {
		cursor = ScheduleCursor{Policy: policy, NextRunAt: e.now().Add(time.Duration(math.Round(w.Trigger.IntervalHours*60)) * time.Minute)}
		if w.Trigger.StartAt != nil {
			cursor.NextRunAt = w.Trigger.StartAt.UTC()
		}
	}
	return cursor
}
func (e *Engine) ScheduleStatus() ([]ScheduleStatus, error) {
	d, err := e.Store.Load()
	if err != nil {
		return nil, err
	}
	s, err := e.load()
	if err != nil {
		return nil, err
	}
	out := []ScheduleStatus{}
	for _, w := range d.Workflows {
		if w.Trigger.Event != "schedule" {
			continue
		}
		next := s.Schedules[w.ID].NextRunAt
		if s.Schedules[w.ID].Policy != schedulePolicy(w) {
			next = time.Time{}
			if w.Trigger.StartAt != nil {
				next = *w.Trigger.StartAt
			}
		}
		out = append(out, ScheduleStatus{w.ID, next, w.Enabled && e.Enabled() && e.Allowed(w.Repository)})
	}
	return out, nil
}
func (e *Engine) tickSchedule(ctx context.Context, w Definition, snapshot State) error {
	if !e.StillEnabled(w) {
		return nil
	}
	// Only Tick advances a cursor. Avoid an active-task lookup until it is due.
	cursor := e.scheduleCursor(w, snapshot)
	now := e.now()
	due := !now.Before(cursor.NextRunAt)
	if !due && snapshot.Schedules[w.ID] == cursor {
		return nil
	}
	active := due && e.Active != nil && e.Active(w)
	var run Run
	err := e.transaction(func(s *State) error {
		if s.Schedules == nil {
			s.Schedules = map[string]ScheduleCursor{}
		}
		s.Schedules[w.ID] = cursor
		item := Item{Repository: w.Repository, Kind: "schedule", Title: w.Name}
		if !due || active || s.busy(item) {
			return nil
		}
		occurrence := cursor.NextRunAt
		cursor.NextRunAt = now.Add(time.Duration(math.Round(w.Trigger.IntervalHours*60)) * time.Minute)
		s.Schedules[w.ID] = cursor
		run = Run{ID: digest([]any{w.ID, cursor.Policy, occurrence}), Workflow: w, Item: item, Status: "dispatching", Reason: "Schedule due at " + occurrence.Format(time.RFC3339), StartedAt: &now, UpdatedAt: now}
		s.Runs = append(s.Runs, run)
		return nil
	})
	if err != nil || run.ID == "" {
		return err
	}
	return e.submitReserved(ctx, run)
}
