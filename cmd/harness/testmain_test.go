package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	authRoot, err := os.MkdirTemp("", "agent00-test-auth-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CODEX_HOME", filepath.Join(authRoot, "codex"))
	_ = os.Setenv("HARNESS_ALLOW_NON_MOLTEN_HUB_BASE_URL", "1")
	_ = os.Setenv("HARNESS_AGENT_HARNESS", "")
	_ = os.Setenv("HARNESS_AGENT_COMMAND", "")
	_ = os.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")
	_ = os.Setenv("MOLTEN_HUB_TOKEN", "")
	_ = os.Setenv("MOLTEN_HUB_REGION", "")
	_ = os.Setenv("MOLTEN_HUB_URL", "")
	code := m.Run()
	_ = os.RemoveAll(authRoot)
	os.Exit(code)
}
