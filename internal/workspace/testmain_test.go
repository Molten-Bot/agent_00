package workspace

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Unsetenv(workspaceRAMBaseEnv)
	_ = os.Unsetenv(workspaceDiskBaseEnv)
	os.Exit(m.Run())
}
