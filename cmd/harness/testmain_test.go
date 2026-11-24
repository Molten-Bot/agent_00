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
	_ = os.Setenv("HARNESS_AGENT_HARNESS", "")
	_ = os.Setenv("HARNESS_AGENT_COMMAND", "")
	// CLI tests must not read or overwrite runtime state left in the source
	// checkout by a real invocation or another test process.
	_ = os.Setenv("HARNESS_RUNTIME_CONFIG_PATH", filepath.Join(authRoot, "runtime-config.json"))
	for _, key := range []string{
		"HARNESS_COMMIT_SIGNING_PRIVATE_KEY",
		"HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE",
		"HARNESS_COMMIT_SIGNING_PASSPHRASE",
	} {
		_ = os.Unsetenv(key)
	}
	code := m.Run()
	_ = os.RemoveAll(authRoot)
	os.Exit(code)
}
