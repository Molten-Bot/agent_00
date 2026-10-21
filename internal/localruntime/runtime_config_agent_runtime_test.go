package localruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveRuntimeConfigAgentRuntimeRejectsHarnessSwitch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"agent_harness":"claude","agent_command":"claude"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := SaveRuntimeConfigAgentRuntime(path, InitConfig{}, "codex", "")
	if err == nil {
		t.Fatal("SaveRuntimeConfigAgentRuntime() error = nil, want non-nil")
	}
	if got := strings.ToLower(err.Error()); !strings.Contains(got, "already configured") || !strings.Contains(got, "agent harness") {
		t.Fatalf("SaveRuntimeConfigAgentRuntime() error = %q, want harness lock guidance", err)
	}
}

func TestSaveRuntimeConfigAgentRuntimeRejectsCommandSwitchForBoundHarness(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"agent_harness":"claude","agent_command":"claude-custom"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := SaveRuntimeConfigAgentRuntime(path, InitConfig{}, "claude", "claude")
	if err == nil {
		t.Fatal("SaveRuntimeConfigAgentRuntime() error = nil, want non-nil")
	}
	if got := strings.ToLower(err.Error()); !strings.Contains(got, "already configured") || !strings.Contains(got, "agent command") {
		t.Fatalf("SaveRuntimeConfigAgentRuntime() error = %q, want command lock guidance", err)
	}
}

func TestSaveRuntimeConfigAgentRuntimeAllowsIdempotentResave(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"agent_harness":"codex","agent_command":"codex"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := SaveRuntimeConfigAgentRuntime(path, InitConfig{}, "codex", ""); err != nil {
		t.Fatalf("SaveRuntimeConfigAgentRuntime() error = %v", err)
	}
}
