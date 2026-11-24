package workflow

import (
	"context"
	"fmt"
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
	e.mu.Lock()
	defer e.mu.Unlock()
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
func (e *Engine) tickSchedule(ctx context.Context, w Definition, s *State, busy map[string]bool) error {
	if s.Schedules == nil {
		s.Schedules = map[string]ScheduleCursor{}
	}
	cursor := e.scheduleCursor(w, *s)
	s.Schedules[w.ID] = cursor
	now := e.now()
	entity := strings.ToLower(w.Repository) + "#0"
	if now.Before(cursor.NextRunAt) || busy[entity] || (e.Active != nil && e.Active(w)) {
		return nil
	}
	occurrence := cursor.NextRunAt
	// Persist the occurrence and advance its cursor together before admission.
	// Missed intervals coalesce into one run; failure retries at the next interval.
	cursor.NextRunAt = now.Add(time.Duration(math.Round(w.Trigger.IntervalHours*60)) * time.Minute)
	s.Schedules[w.ID] = cursor
	r := Run{ID: digest([]any{w.ID, cursor.Policy, occurrence}), Workflow: w, Item: Item{Repository: w.Repository, Kind: "schedule", Title: w.Name}, Status: "dispatching", Reason: "Schedule due at " + occurrence.Format(time.RFC3339), StartedAt: &now, UpdatedAt: now}
	s.Runs = append(s.Runs, r)
	if err := e.save(*s); err != nil {
		return err
	}
	e.logRun(r, "dispatch")
	var id string
	var err error
	if !e.StillEnabled(w) {
		err = fmt.Errorf("workflow changed or was disabled before scheduled submission")
	} else {
		id, err = e.Submit(ctx, r)
	}
	index := len(s.Runs) - 1
	if err != nil {
		s.Runs[index].Status = "failed"
		s.Runs[index].Reason = "Scheduled submission failed: " + err.Error()
	} else {
		s.Runs[index].Status = "running"
		s.Runs[index].RequestID = id
		busy[entity] = true
	}
	if err := e.save(*s); err != nil {
		return err
	}
	e.logRun(s.Runs[index], "submission")
	return nil
}
