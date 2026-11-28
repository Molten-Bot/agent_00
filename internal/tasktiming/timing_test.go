package tasktiming

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAttemptTimingSeparatesQueueAgentAndOtherWork(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	now := start
	var log strings.Builder
	logf := func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }
	ctx := WithRecorder(t.Context(), "task", "first", func() time.Time { return now }, logf)
	advance := func(seconds int) { now = now.Add(time.Duration(seconds) * time.Second) }

	Accepted(ctx, now)
	advance(12)
	Set(ctx, Implementation)
	advance(3)
	first := StartAgent(AgentCommand(ctx))
	advance(5)
	first()
	advance(7)
	second := StartAgent(AgentCommand(ctx))
	advance(11)
	second()
	Set(ctx, "")
	retry := WithRecorder(t.Context(), "task", "retry", func() time.Time { return now }, logf)
	Accepted(retry, now)
	advance(4)
	Set(retry, Checks)
	advance(20)
	Set(retry, "")

	spans, err := Read(t.Context(), strings.NewReader(log.String()), "task")
	if err != nil || len(spans) != 4 {
		t.Fatalf("read %d spans, err = %v, log:\n%s", len(spans), err, log.String())
	}
	for i, want := range []struct {
		attempt string
		stage   Stage
		wallMS  int64
	}{
		{"first", Queue, 12000}, {"first", Implementation, 26000},
		{"retry", Queue, 4000}, {"retry", Checks, 20000},
	} {
		span := spans[i]
		if span.Attempt != want.attempt || span.Stage != want.stage || span.WallMS == nil || *span.WallMS != want.wallMS {
			t.Errorf("span %d = %+v, want %+v", i, span, want)
		}
	}
	if spans[1].AgentMS == nil || *spans[1].AgentMS != 16000 || spans[0].AgentMS != nil || spans[3].AgentMS != nil {
		t.Fatalf("agent execution was not isolated:\n%s", log.String())
	}
	if !start.Equal(*spans[0].StartedAt) || !spans[0].EndedAt.Equal(*spans[1].StartedAt) {
		t.Fatal("admission and implementation boundary did not match")
	}
}

func TestReadTimingWithMissingBoundaries(t *testing.T) {
	t.Parallel()
	input := strings.Repeat("x", 10000) + "\n" +
		"task_timing request_id=other attempt=a span=1 stage=implementation started_at=unknown ended_at=unknown wall_ms=unknown agent_ms=unknown\n" +
		"task_timing request_id=task attempt=a span=1 stage=implementation started_at=2026-11-27T12:00:00Z ended_at=unknown wall_ms=unknown agent_ms=unknown\n" +
		"task_timing request_id=task attempt=b span=1 stage=merge started_at=unknown ended_at=2026-11-27T12:00:00Z wall_ms=unknown agent_ms=unknown\n"
	spans, err := Read(t.Context(), strings.NewReader(input), "task")
	if err != nil || len(spans) != 2 || spans[0].StartedAt == nil || spans[0].EndedAt != nil ||
		spans[1].StartedAt != nil || spans[1].EndedAt == nil || spans[0].WallMS != nil || spans[1].WallMS != nil {
		t.Fatalf("restored spans = %+v, err = %v", spans, err)
	}
}
