package localruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalConfigDefaultsWithoutRemoteCredentials(t *testing.T) {
	t.Parallel()
	cfg := InitConfig{}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler.MaxParallel < 1 || cfg.LogLevel != DefaultLogLevel || cfg.ReviewWatch.ReviewCount() != 0 {
		t.Fatalf("unexpected local defaults: %#v", cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"base_url", "bind_token", "agent_token", "session_key", "profile", "handle"} {
		if _, exists := doc[key]; exists {
			t.Errorf("local configuration contains retired key %q", key)
		}
	}
}

func TestSaveLocalSettingsDiscardsRetiredCredentials(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	initial := `{"base_url":"https://retired.example/v1","agent_token":"unused","bind_token":"unused","session_key":"old","profile":{},"github_token":"preserved","agent_harness":"codex","library_task_usage":{"code-review":2}}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{ReviewLevel: "low"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubToken != "preserved" || cfg.AgentHarness != "codex" || cfg.ReviewWatch.ReviewCount() != 1 || cfg.LibraryTaskUsage["code-review"] != 2 {
		t.Fatal("local settings were lost during save")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"base_url", "agent_token", "bind_token", "session_key", "profile"} {
		if _, exists := doc[key]; exists {
			t.Errorf("saved configuration retained %q", key)
		}
	}
}

func TestDefaultLibraryEnabledDefaultsTrueAndRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")

	if !ReadRuntimeConfigDefaultLibraryEnabled(path) {
		t.Fatal("default library should default to enabled before any config file exists")
	}

	disabled := false
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{}, &disabled, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ReadRuntimeConfigDefaultLibraryEnabled(path) {
		t.Fatal("default library should be disabled after saving false")
	}

	enabled := true
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{}, &enabled, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !ReadRuntimeConfigDefaultLibraryEnabled(path) {
		t.Fatal("default library should be enabled after saving true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, exists := doc["default_library_enabled"]; exists {
		t.Error("saving the default (enabled) value should omit the key, keeping the file minimal")
	}
}

func TestChatEnabledDefaultsFalseAndRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")

	if ReadRuntimeConfigChatEnabled(path) {
		t.Fatal("chat should default to disabled before any config file exists")
	}

	enabled := true
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{}, nil, &enabled, nil); err != nil {
		t.Fatal(err)
	}
	if !ReadRuntimeConfigChatEnabled(path) {
		t.Fatal("chat should be enabled after saving true")
	}

	disabled := false
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{}, nil, &disabled, nil); err != nil {
		t.Fatal(err)
	}
	if ReadRuntimeConfigChatEnabled(path) {
		t.Fatal("chat should be disabled after saving false")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, exists := doc["chat_enabled"]; exists {
		t.Error("saving the default (disabled) value should omit the key, keeping the file minimal")
	}
}

func TestJSONEnabledDefaultsFalseAndRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")

	if ReadRuntimeConfigJSONEnabled(path) {
		t.Fatal("json input should default to disabled before any config file exists")
	}

	enabled := true
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{}, nil, nil, &enabled); err != nil {
		t.Fatal(err)
	}
	if !ReadRuntimeConfigJSONEnabled(path) {
		t.Fatal("json input should be enabled after saving true")
	}

	disabled := false
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{}, nil, nil, &disabled); err != nil {
		t.Fatal(err)
	}
	if ReadRuntimeConfigJSONEnabled(path) {
		t.Fatal("json input should be disabled after saving false")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, exists := doc["json_enabled"]; exists {
		t.Error("saving the default (disabled) value should omit the key, keeping the file minimal")
	}
}
