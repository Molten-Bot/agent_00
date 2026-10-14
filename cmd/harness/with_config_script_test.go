package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWithConfigScriptPrefersRunConfig(t *testing.T) {
	// Keep with-config script tests serial: parallel execution intermittently
	// triggers ETXTBSY ("Text file busy") when invoking the stub harness.
	env := newWithConfigTestEnv(t)
	configPath := filepath.Join(env.configDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"repo":"git@github.com:acme/repo.git","prompt":"x"}`), 0o644); err != nil {
		t.Fatalf("write run config: %v", err)
	}

	output, err := runWithConfigScript(t, env, nil)
	if err != nil {
		t.Fatalf("with-config error: %v\noutput: %s", err, output)
	}

	args := readFileTrimmed(t, env.argsPath)
	if got, want := args, "run --config "+configPath; got != want {
		t.Fatalf("harness args = %q, want %q", got, want)
	}
}

func TestWithConfigScriptUsesLocalRuntimeConfig(t *testing.T) {
	env := newWithConfigTestEnv(t)
	configPath := filepath.Join(env.configDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"agent_harness":"codex"}`), 0o644); err != nil {
		t.Fatalf("write hub config: %v", err)
	}

	output, err := runWithConfigScript(t, env, nil)
	if err != nil {
		t.Fatalf("with-config error: %v\noutput: %s", err, output)
	}

	args := readFileTrimmed(t, env.argsPath)
	if got, want := args, "serve --config "+configPath+" --ui-listen :7777"; got != want {
		t.Fatalf("harness args = %q, want %q", got, want)
	}
}

func TestWithConfigScriptFallsBackToInitConfig(t *testing.T) {
	env := newWithConfigTestEnv(t)
	initPath := filepath.Join(env.configDir, "init.json")
	if err := os.WriteFile(initPath, []byte(`{"agent_harness":"codex"}`), 0o644); err != nil {
		t.Fatalf("write init config: %v", err)
	}

	output, err := runWithConfigScript(t, env, nil)
	if err != nil {
		t.Fatalf("with-config error: %v\noutput: %s", err, output)
	}

	args := readFileTrimmed(t, env.argsPath)
	if got, want := args, "serve --init "+initPath+" --ui-listen :7777"; got != want {
		t.Fatalf("harness args = %q, want %q", got, want)
	}
}

func TestWithConfigScriptMissingConfigStartsLocalOnboardingMode(t *testing.T) {
	env := newWithConfigTestEnv(t)
	output, err := runWithConfigScript(t, env, nil)
	if err != nil {
		t.Fatalf("with-config error: %v\noutput: %s", err, output)
	}

	args := readFileTrimmed(t, env.argsPath)
	if got, want := args, "serve --ui-listen :7777"; got != want {
		t.Fatalf("harness args = %q, want %q", got, want)
	}

	for _, want := range []string{
		"starting local onboarding with defaults",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q\noutput: %s", want, output)
		}
	}
}

func TestWithConfigScriptAllowsUIListenOverride(t *testing.T) {
	env := newWithConfigTestEnv(t)
	output, err := runWithConfigScript(t, env, map[string]string{
		"HARNESS_UI_LISTEN": ":8088",
	})
	if err != nil {
		t.Fatalf("with-config error: %v\noutput: %s", err, output)
	}

	args := readFileTrimmed(t, env.argsPath)
	if got, want := args, "serve --ui-listen :8088"; got != want {
		t.Fatalf("harness args = %q, want %q", got, want)
	}
}

type withConfigTestEnv struct {
	root      string
	configDir string
	argsPath  string
	initPath  string
}

func newWithConfigTestEnv(t *testing.T) withConfigTestEnv {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}

	argsPath := filepath.Join(root, "harness.args")
	initPath := filepath.Join(root, "harness.init.json")
	stubPath := filepath.Join(binDir, "harness")
	stub := `#!/bin/sh
set -eu
printf '%s' "$*" > "${HARNESS_STUB_ARGS_FILE}"
if [ "${1:-}" = "hub" ] && [ "${2:-}" = "--init" ] && [ "${3:-}" != "" ]; then
    cat "${3}" > "${HARNESS_STUB_INIT_FILE}"
fi
if [ "${1:-}" = "hub" ] && [ "${2:-}" = "--config" ] && [ "${3:-}" != "" ]; then
    cat "${3}" > "${HARNESS_STUB_INIT_FILE}"
fi
`
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatalf("write harness stub: %v", err)
	}

	return withConfigTestEnv{
		root:      root,
		configDir: configDir,
		argsPath:  argsPath,
		initPath:  initPath,
	}
}

func runWithConfigScript(t *testing.T, env withConfigTestEnv, extra map[string]string) (string, error) {
	t.Helper()

	cmd := exec.Command("sh", withConfigScriptPath(t))
	pathValue := filepath.Join(env.root, "bin") + ":" + os.Getenv("PATH")
	cmd.Env = []string{
		"PATH=" + pathValue,
		"HARNESS_CONFIG_DIR=" + env.configDir,
		"HARNESS_STUB_ARGS_FILE=" + env.argsPath,
		"HARNESS_STUB_INIT_FILE=" + env.initPath,
	}
	for key, value := range extra {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	output, err := cmd.CombinedOutput()
	return string(output), err
}

func withConfigScriptPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return filepath.Join(root, "docker", "with-config.sh")
}

func readFileTrimmed(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}
