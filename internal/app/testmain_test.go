package app

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Unsetenv("GH_TOKEN")
	_ = os.Unsetenv("GITHUB_TOKEN")
	_ = os.Unsetenv("HARNESS_WORKSPACE_RAM_BASE")
	_ = os.Unsetenv("HARNESS_WORKSPACE_DISK_BASE")
	os.Exit(m.Run())
}
