package hub

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("HARNESS_ALLOW_NON_MOLTEN_HUB_BASE_URL", "1")
	_ = os.Setenv("HARNESS_AGENT_HARNESS", "")
	_ = os.Setenv("HARNESS_AGENT_COMMAND", "")
	_ = os.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")
	_ = os.Setenv("MOLTEN_HUB_TOKEN", "")
	_ = os.Setenv("MOLTEN_HUB_REGION", "")
	_ = os.Setenv("MOLTEN_HUB_URL", "")
	os.Exit(m.Run())
}
