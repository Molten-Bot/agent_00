package main

import (
	"testing"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
)

func TestLibraryScheduleWaitsForManualAndPausedRuns(t *testing.T) {
	t.Parallel()
	broker := web.NewBroker()
	schedule := localruntime.LibrarySchedule{Key: "unit-test-coverage@acme/repo"}
	broker.RecordTaskRunConfigWithSource("manual", []byte(`{"repos":["git@github.com:Acme/Repo.git"],"libraryTaskName":"unit-test-coverage","baseBranch":"other-branch"}`), "library")
	for _, line := range []string{"", "task status=start request_id=manual", "task status=paused request_id=manual"} {
		if line != "" {
			broker.IngestLog(line)
		}
		if !libraryScheduleActive(broker, schedule) {
			t.Fatalf("overlap allowed after %q", line)
		}
	}
	if libraryScheduleActive(broker, localruntime.LibrarySchedule{Key: "security-secrets@acme/repo"}) {
		t.Fatal("unrelated task blocked")
	}
	if libraryScheduleActive(broker, localruntime.LibrarySchedule{Key: "unit-test-coverage@acme/other"}) {
		t.Fatal("unrelated repo blocked")
	}
	broker.IngestLog("task status=completed request_id=manual")
	if libraryScheduleActive(broker, schedule) {
		t.Fatal("completed run blocks future occurrences")
	}
}
