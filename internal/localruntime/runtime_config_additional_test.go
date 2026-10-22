package localruntime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestLoadRuntimeConfigRejectsInvalidScheduler(t *testing.T) {
	t.Parallel()

	// cpu_high_watermark > 100 survives ApplyDefaults (which only backfills
	// non-positive values) and is rejected by Validate, exercising
	// RuntimeConfig.Validate's error-propagation branch end-to-end.
	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":"v1","scheduler":{"cpu_high_watermark":150}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := LoadRuntimeConfig(path); err == nil {
		t.Fatal("LoadRuntimeConfig(invalid scheduler) error = nil, want validation error")
	} else if !strings.Contains(err.Error(), "scheduler.cpu_high_watermark must be > 0 and <= 100") {
		t.Fatalf("LoadRuntimeConfig() error = %v, want scheduler.cpu_high_watermark validation error", err)
	}
}

func TestRuntimeConfigReviewDocOnlyDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  map[string]any
		want bool
	}{
		{"empty doc", map[string]any{}, true},
		{"all default values", map[string]any{
			"poll_interval_ms": float64(60000),
			"writeback":        "summary-comment",
			"response_mode":    "off",
			"review_level":     "off",
		}, true},
		{"integer poll interval default", map[string]any{"poll_interval_ms": 60000}, true},
		{"non-default poll interval", map[string]any{"poll_interval_ms": float64(30000)}, false},
		{"malformed poll interval", map[string]any{"poll_interval_ms": "soon"}, false},
		{"non-default writeback", map[string]any{"writeback": "off"}, false},
		{"non-default response mode", map[string]any{"response_mode": "caveman-full"}, false},
		{"non-default review level", map[string]any{"review_level": "high"}, false},
		{"unknown key", map[string]any{"merge_method": "squash"}, false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runtimeConfigReviewDocOnlyDefaults(tc.doc); got != tc.want {
				t.Fatalf("runtimeConfigReviewDocOnlyDefaults(%#v) = %v, want %v", tc.doc, got, tc.want)
			}
		})
	}
}

func TestWriteRuntimeConfigFileRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "config.json")
	payload := []byte(`{"github_token":"saved"}` + "\n")
	if err := writeRuntimeConfigFile(path, payload); err != nil {
		t.Fatalf("writeRuntimeConfigFile() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round-tripped content = %q, want %q", got, payload)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm = %o, want 0600", perm)
	}

	// Overwriting an existing file should still succeed and replace contents.
	payload2 := []byte(`{"github_token":"updated"}` + "\n")
	if err := writeRuntimeConfigFile(path, payload2); err != nil {
		t.Fatalf("writeRuntimeConfigFile(overwrite) error = %v", err)
	}
	got2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(after overwrite) error = %v", err)
	}
	if string(got2) != string(payload2) {
		t.Fatalf("round-tripped content after overwrite = %q, want %q", got2, payload2)
	}
}

func TestWriteRuntimeConfigFileFailsWhenParentIsNotADirectory(t *testing.T) {
	t.Parallel()

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(blocker) error = %v", err)
	}

	path := filepath.Join(blocker, "sub", "config.json")
	if err := writeRuntimeConfigFile(path, []byte("{}")); err == nil {
		t.Fatal("writeRuntimeConfigFile(path under a file) error = nil, want non-nil")
	} else if !strings.Contains(err.Error(), "create runtime config dir") {
		t.Fatalf("writeRuntimeConfigFile() error = %v, want mkdir failure", err)
	}
}

func TestWriteRuntimeConfigFileFailsWhenDirNotWritable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	path := filepath.Join(dir, "config.json")
	if err := writeRuntimeConfigFile(path, []byte("{}")); err == nil {
		t.Fatal("writeRuntimeConfigFile(read-only dir) error = nil, want non-nil")
	} else if !strings.Contains(err.Error(), "create temp runtime config") {
		t.Fatalf("writeRuntimeConfigFile() error = %v, want temp file creation failure", err)
	}
}

func TestSaveRuntimeConfigClaudeOAuthTokenCreatesConfigWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(pre-save) error = %v, want not-exist", err)
	}

	if err := SaveRuntimeConfigClaudeOAuthToken(path, InitConfig{Version: "v1"}, "oauth_token_fresh"); err != nil {
		t.Fatalf("SaveRuntimeConfigClaudeOAuthToken() error = %v", err)
	}

	doc := readJSONDocForTest(t, path)
	if doc["claude_code_oauth_token"] != "oauth_token_fresh" {
		t.Fatalf("claude_code_oauth_token = %#v, want %q", doc["claude_code_oauth_token"], "oauth_token_fresh")
	}

	// Saving again should merge into the config just created rather than
	// clobbering it, confirming the round trip through writeRuntimeConfigDoc.
	if err := SaveRuntimeConfigClaudeOAuthToken(path, InitConfig{Version: "v1"}, "oauth_token_rotated"); err != nil {
		t.Fatalf("SaveRuntimeConfigClaudeOAuthToken(rotate) error = %v", err)
	}
	doc = readJSONDocForTest(t, path)
	if doc["claude_code_oauth_token"] != "oauth_token_rotated" {
		t.Fatalf("claude_code_oauth_token after rotate = %#v, want %q", doc["claude_code_oauth_token"], "oauth_token_rotated")
	}
}

func TestSaveRuntimeConfigClaudeOAuthTokenReportsUnreadablePath(t *testing.T) {
	t.Parallel()

	// A directory at the config path makes os.ReadFile fail with an error other
	// than "not exist", exercising the default/unexpected read-error branch.
	path := t.TempDir()
	err := SaveRuntimeConfigClaudeOAuthToken(path, InitConfig{Version: "v1"}, "oauth_token_saved")
	if err == nil {
		t.Fatal("SaveRuntimeConfigClaudeOAuthToken(directory path) error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "read runtime config") {
		t.Fatalf("SaveRuntimeConfigClaudeOAuthToken() error = %v, want read runtime config failure", err)
	}
}
