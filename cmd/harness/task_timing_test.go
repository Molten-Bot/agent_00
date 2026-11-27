package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/tasktiming"
	"github.com/Molten-Bot/agent_00/internal/web"
)

func TestTaskTimingSurvivesTranscriptCleanupAndRestart(t *testing.T) {
	t.Parallel()
	const id = "local-task"
	root := filepath.Join(t.TempDir(), logDirectoryName)
	mirror, err := newTaskLogMirror(root)
	if err != nil {
		t.Fatal(err)
	}
	logger := newTerminalLogger(io.Discard, false)
	logger.sink = mirror
	broker := web.NewBroker()
	router := newRuntimeLogRouter(logger, broker, localruntime.LogLevelInfo)
	router.Printf("task status=start request_id=%s", id)

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	timingCtx := tasktiming.WithRecorder(t.Context(), id, "first", func() time.Time { return now }, router.Printf)
	tasktiming.Accepted(timingCtx, now)
	now = now.Add(2 * time.Second)
	tasktiming.Set(timingCtx, "")
	task, _ := broker.Task(id)
	if task.Status != "running" || !strings.Contains(task.Logs[len(task.Logs)-1].Text, "wall_ms=2000") {
		t.Fatalf("timing did not appear in task output or affected the task state: %+v", task)
	}
	dir, _ := taskLogDir(root, id)
	transcript, err := os.ReadFile(filepath.Join(dir, logFileName))
	if err != nil || !strings.Contains(string(transcript), "wall_ms=2000") {
		t.Fatalf("wall timing missing from transcript: %s, err = %v", transcript, err)
	}
	if err := mirror.removeTaskTranscripts(dir); err != nil {
		t.Fatal(err)
	}
	logger.Close()

	restarted, err := newTaskLogMirror(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	spans, err := restarted.TaskTiming(t.Context(), id)
	if err != nil || len(spans) != 1 || spans[0].WallMS == nil || *spans[0].WallMS != 2000 {
		t.Fatalf("restored timeline = %+v, err = %v", spans, err)
	}
}
