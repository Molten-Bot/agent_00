package localruntime

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestAdaptiveSchedulerQueuesWhenWindowsWorkspaceRootIsOutOfSpace(t *testing.T) {
	t.Parallel()

	t.Run("disk_full", func(t *testing.T) {
		testAdaptiveSchedulerQueuesWhenWorkspaceRootIsOutOfSpace(t, windows.ERROR_DISK_FULL)
	})
	t.Run("handle_disk_full", func(t *testing.T) {
		testAdaptiveSchedulerQueuesWhenWorkspaceRootIsOutOfSpace(t, windows.ERROR_HANDLE_DISK_FULL)
	})
}
