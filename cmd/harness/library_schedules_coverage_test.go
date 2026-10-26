package main

import (
	"testing"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
)

func TestLibraryScheduleActiveSkipsTasksMissingRunConfig(t *testing.T) {
	t.Parallel()

	broker := web.NewBroker()
	broker.IngestLog("task status=start request_id=no-run-config")

	schedule := localruntime.LibrarySchedule{Key: "unit-test-coverage@acme/repo"}
	if libraryScheduleActive(broker, schedule) {
		t.Fatal("libraryScheduleActive() = true for task with no recorded run config, want false")
	}
}

func TestLibraryScheduleActiveSkipsTasksWithMalformedRunConfig(t *testing.T) {
	t.Parallel()

	broker := web.NewBroker()
	broker.IngestLog("task status=start request_id=bad-json")
	broker.RecordTaskRunConfigWithSource("bad-json", []byte("not valid json"), "library")

	schedule := localruntime.LibrarySchedule{Key: "unit-test-coverage@acme/repo"}
	if libraryScheduleActive(broker, schedule) {
		t.Fatal("libraryScheduleActive() = true for task with malformed run config, want false")
	}
}
