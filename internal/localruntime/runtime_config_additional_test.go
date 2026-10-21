package localruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultRuntimeConfigPathUsesEnvOverride(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "/tmp/runtime-config.json")

	if got, want := defaultRuntimeConfigPath(), "/tmp/runtime-config.json"; got != want {
		t.Fatalf("defaultRuntimeConfigPath() = %q, want %q", got, want)
	}
}

func TestResolveRuntimeConfigPathEmptyInitUsesDefault(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")

	if got, want := ResolveRuntimeConfigPath(""), runtimeConfigPath; got != want {
		t.Fatalf("ResolveRuntimeConfigPath(\"\") = %q, want %q", got, want)
	}
}

func TestSaveRuntimeConfigGitHubTokenMergesIntoExistingConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"custom":"preserved"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := SaveRuntimeConfigGitHubToken(path, InitConfig{}, "github_token_saved_token"); err != nil {
		t.Fatalf("SaveRuntimeConfigGitHubToken() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got["github_token"] != "github_token_saved_token" {
		t.Fatalf("github_token = %#v, want %q", got["github_token"], "github_token_saved_token")
	}
	if got["custom"] != "preserved" {
		t.Fatalf("custom = %#v, want %q", got["custom"], "preserved")
	}
}

func readJSONDocForTest(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return doc
}

func TestReadRuntimeConfigString(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"github_token":"github_token_saved","openai_api_key":"sk_saved"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if got, want := ReadRuntimeConfigString(path, "github_token", "githubToken"), "github_token_saved"; got != want {
		t.Fatalf("ReadRuntimeConfigString(github_token) = %q, want %q", got, want)
	}
	if got, want := ReadRuntimeConfigString(path, "missing", "openai_api_key"), "sk_saved"; got != want {
		t.Fatalf("ReadRuntimeConfigString(openai_api_key) = %q, want %q", got, want)
	}
}

func TestReadRuntimeConfigStringInvalidInputs(t *testing.T) {
	t.Parallel()

	if got := ReadRuntimeConfigString("", "github_token"); got != "" {
		t.Fatalf("ReadRuntimeConfigString(empty path) = %q, want empty", got)
	}
	if got := ReadRuntimeConfigString(filepath.Join(t.TempDir(), "missing.json"), "github_token"); got != "" {
		t.Fatalf("ReadRuntimeConfigString(missing path) = %q, want empty", got)
	}

	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if got := ReadRuntimeConfigString(path, "github_token"); got != "" {
		t.Fatalf("ReadRuntimeConfigString(malformed) = %q, want empty", got)
	}
}

func TestLoadRuntimeConfigBackfillsMissingLogLevel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if got.LogLevel != DefaultLogLevel {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, DefaultLogLevel)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if doc["log_level"] != DefaultLogLevel {
		t.Fatalf("log_level = %#v, want %q", doc["log_level"], DefaultLogLevel)
	}
}

func TestLoadRuntimeConfigHonorsConfiguredLogLevel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"log_level":"debug"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if got.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, "debug")
	}
}
