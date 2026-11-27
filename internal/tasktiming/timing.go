// Package tasktiming records task stage boundaries in the existing task log.
package tasktiming

import (
	"context"
	"crypto/rand"
	"strconv"
	"time"
)

type Stage string

const (
	Queue          Stage = "queue"
	App            Stage = "app"
	Implementation Stage = "implementation"
	Publication    Stage = "pr_publication"
	Checks         Stage = "ci_wait"
	Review         Stage = "review"
	Repair         Stage = "repair"
	Verification   Stage = "draft_verification"
	Ready          Stage = "ready"
	Merge          Stage = "merge"
	GitHub         Stage = "github_wait"
	MergeWait      Stage = "merge_wait"
	WorkflowWait   Stage = "workflow_wait"
	WorkflowTotal  Stage = "workflow_total"
)

// Span IDs are local to an attempt. Worker intervals are exclusive. The
// workflow coordinator uses independent IDs for its remote waits and actions.
type Span struct {
	Attempt   string     `json:"attempt"`
	ID        string     `json:"id"`
	Stage     Stage      `json:"stage"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	WallMS    *int64     `json:"wall_ms"`
	AgentMS   *int64     `json:"agent_ms"`
}

type Logf func(string, ...any)

func (s Span) Write(logf Logf, requestID string) {
	if logf == nil || requestID == "" {
		return
	}
	logf("task_timing request_id=%s attempt=%s span=%s stage=%s started_at=%s ended_at=%s wall_ms=%s agent_ms=%s",
		requestID, s.Attempt, s.ID, s.Stage, timestamp(s.StartedAt), timestamp(s.EndedAt), milliseconds(s.WallMS), milliseconds(s.AgentMS))
}

func (s Span) End(at time.Time) Span {
	s.EndedAt = timePointer(at)
	if s.StartedAt != nil && s.EndedAt != nil && !at.Before(*s.StartedAt) {
		elapsed := at.Sub(*s.StartedAt).Milliseconds()
		s.WallMS = &elapsed
	}
	return s
}

func Start(attempt, id string, stage Stage, at time.Time) Span {
	return Span{Attempt: attempt, ID: id, Stage: stage, StartedAt: timePointer(at)}
}

func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func timestamp(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Format(time.RFC3339Nano)
}

func milliseconds(ms *int64) string {
	if ms == nil {
		return "unknown"
	}
	return strconv.FormatInt(*ms, 10)
}

type recorderKey struct{}
type agentKey struct{}

type recorder struct {
	requestID     string
	attempt       string
	now           func() time.Time
	logf          Logf
	sequence      int
	prefix        string
	current       Span
	agentElapsed  time.Duration
	agentMeasured bool
}

func WithRecorder(ctx context.Context, requestID, attempt string, now func() time.Time, logf Logf) context.Context {
	if logf == nil || requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, &recorder{requestID: requestID, attempt: attempt, now: now, logf: logf, prefix: rand.Text() + "-"})
}

func Accepted(ctx context.Context, at time.Time) {
	if r, _ := ctx.Value(recorderKey{}).(*recorder); r != nil {
		r.setAt(Queue, at)
	}
}

func Set(ctx context.Context, stage Stage) {
	r, _ := ctx.Value(recorderKey{}).(*recorder)
	if r != nil {
		r.set(stage)
	}
}

// Enter temporarily replaces the current stage, so nested waits remain
// visible without inflating the enclosing stage's elapsed time.
func Enter(ctx context.Context, stage Stage) func() {
	r, _ := ctx.Value(recorderKey{}).(*recorder)
	if r == nil || r.current.Stage == stage {
		return func() {}
	}
	previous := r.current.Stage
	r.set(stage)
	return func() { r.set(previous) }
}

func (r *recorder) set(stage Stage) {
	r.setAt(stage, r.now())
}

func (r *recorder) setAt(stage Stage, at time.Time) {
	if r.current.Stage == stage {
		return
	}
	if r.current.Stage != "" {
		span := r.current.End(at)
		if r.agentMeasured {
			ms := r.agentElapsed.Milliseconds()
			span.AgentMS = &ms
		}
		span.Write(r.logf, r.requestID)
	}
	r.current = Span{}
	r.agentElapsed, r.agentMeasured = 0, false
	if stage != "" {
		r.sequence++
		r.current = Start(r.attempt, r.prefix+strconv.Itoa(r.sequence), stage, at)
		r.current.Write(r.logf, r.requestID)
	}
}

// AgentCommand marks only the agent invocation, excluding authentication,
// model selection and other preparation around it.
func AgentCommand(ctx context.Context) context.Context {
	if ctx.Value(recorderKey{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, agentKey{}, true)
}

// StartAgent measures the runner call only for explicitly marked invocations.
func StartAgent(ctx context.Context) func() {
	if ctx.Value(agentKey{}) != true {
		return func() {}
	}
	r := ctx.Value(recorderKey{}).(*recorder)
	start := r.now()
	return func() {
		end := r.now()
		if !start.IsZero() && !end.IsZero() && !end.Before(start) {
			r.agentMeasured = true
			r.agentElapsed += end.Sub(start)
		}
	}
}
