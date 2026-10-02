package hub

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "agent00-hub-tests-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HARNESS_WORKSPACE_RAM_BASE", root)
	_ = os.Setenv("HARNESS_WORKSPACE_DISK_BASE", root)
	_ = os.Unsetenv("HARNESS_WORKSPACE_ROOT_NAME")
	_ = os.Setenv("HARNESS_ALLOW_NON_MOLTEN_HUB_BASE_URL", "1")
	_ = os.Setenv("HARNESS_AGENT_HARNESS", "")
	_ = os.Setenv("HARNESS_AGENT_COMMAND", "")
	_ = os.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")
	_ = os.Setenv("MOLTEN_HUB_TOKEN", "")
	_ = os.Setenv("MOLTEN_HUB_REGION", "")
	_ = os.Setenv("MOLTEN_HUB_URL", "")
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
