package tasktiming

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// Read folds start and finish entries from a task log into stage intervals.
// A finish record carries both timestamps so it can also stand on its own.
func Read(ctx context.Context, input io.Reader, requestID string) ([]Span, error) {
	spans := []Span{}
	indices := map[[2]string]int{}
	reader := bufio.NewReaderSize(input, 4096)
	discard := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			discard = true
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if !discard && len(line) > 0 && line[len(line)-1] == '\n' {
			if span, ok := parseSpan(string(line), requestID); ok {
				key := [2]string{span.Attempt, span.ID}
				index, exists := indices[key]
				if !exists {
					indices[key] = len(spans)
					spans = append(spans, span)
				} else if span.EndedAt != nil {
					spans[index] = span
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return spans, nil
		}
		discard = false
	}
}

func parseSpan(line, requestID string) (Span, bool) {
	prefix := "task_timing request_id=" + requestID + " "
	if !strings.HasPrefix(line, prefix) {
		return Span{}, false
	}
	var span Span
	for field := range strings.FieldsSeq(line[len(prefix):]) {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "attempt":
			span.Attempt = value
		case "span":
			span.ID = value
		case "stage":
			span.Stage = Stage(value)
		case "started_at":
			span.StartedAt = parseTimestamp(value)
		case "ended_at":
			span.EndedAt = parseTimestamp(value)
		case "wall_ms":
			span.WallMS = parseMilliseconds(value)
		case "agent_ms":
			span.AgentMS = parseMilliseconds(value)
		}
	}
	if span.StartedAt == nil || span.EndedAt == nil {
		span.WallMS, span.AgentMS = nil, nil
	}
	return span, span.Attempt != "" && span.ID != "" && span.Stage != ""
}

func parseTimestamp(value string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return timePointer(t)
}

func parseMilliseconds(value string) *int64 {
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ms < 0 {
		return nil
	}
	return &ms
}
