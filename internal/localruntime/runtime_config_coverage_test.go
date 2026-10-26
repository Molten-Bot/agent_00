package localruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRuntimeConfigRejectsNonObjectJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`[1,2,3]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeConfig(path); err == nil || !strings.Contains(err.Error(), "parse runtime config") {
		t.Fatalf("LoadRuntimeConfig(array) error = %v, want parse runtime config failure", err)
	}
}

func TestLoadRuntimeConfigRejectsTypeMismatchedField(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":123}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeConfig(path); err == nil || !strings.Contains(err.Error(), "parse runtime config") {
		t.Fatalf("LoadRuntimeConfig(type mismatch) error = %v, want parse runtime config failure", err)
	}
}

func TestSaveRuntimeConfigAgentRuntimeDefaultPath(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")
	t.Chdir(t.TempDir())
	if err := SaveRuntimeConfigAgentRuntime("", InitConfig{}, "codex", ""); err != nil {
		t.Fatalf("SaveRuntimeConfigAgentRuntime(empty path) error = %v", err)
	}
	if _, err := os.Stat(runtimeConfigPath); err != nil {
		t.Fatalf("Stat(default runtime config path) error = %v, want file written at default path", err)
	}
}

func TestSaveRuntimeConfigAgentRuntimeRejectsUnsupportedHarness(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveRuntimeConfigAgentRuntime(path, InitConfig{}, "bogus-harness", ""); err == nil {
		t.Fatal("SaveRuntimeConfigAgentRuntime(bogus harness) error = nil, want resolve failure")
	}
}

func TestSaveRuntimeConfigAgentRuntimeRejectsMalformedExistingConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRuntimeConfigAgentRuntime(path, InitConfig{}, "codex", ""); err == nil {
		t.Fatal("SaveRuntimeConfigAgentRuntime(malformed config) error = nil, want load failure")
	}
}

func TestSaveRuntimeConfigAgentRuntimeFallsBackToCamelCaseHarnessKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentHarness":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveRuntimeConfigAgentRuntime(path, InitConfig{}, "codex", "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "claude") {
		t.Fatalf("SaveRuntimeConfigAgentRuntime() error = %v, want harness lock reading camelCase agentHarness", err)
	}
}

func TestSaveRuntimeConfigAgentRuntimeFallsBackToCamelCaseCommandKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agent_harness":"claude","agentCommand":"claude-custom"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveRuntimeConfigAgentRuntime(path, InitConfig{}, "claude", "claude")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "claude-custom") {
		t.Fatalf("SaveRuntimeConfigAgentRuntime() error = %v, want command lock reading camelCase agentCommand", err)
	}
}

func TestSaveRuntimeConfigReviewAndDefaultLibrarySettingsDefaultPath(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")
	t.Chdir(t.TempDir())
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings("", InitConfig{}, ReviewWatchConfig{}, nil, nil, nil); err != nil {
		t.Fatalf("SaveRuntimeConfigReviewAndDefaultLibrarySettings(empty path) error = %v", err)
	}
	if _, err := os.Stat(runtimeConfigPath); err != nil {
		t.Fatalf("Stat(default runtime config path) error = %v, want file written at default path", err)
	}
}

func TestSaveRuntimeConfigReviewAndDefaultLibrarySettingsRejectsMalformedExistingConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{}, nil, nil, nil); err == nil {
		t.Fatal("SaveRuntimeConfigReviewAndDefaultLibrarySettings(malformed config) error = nil, want load failure")
	}
}

func TestSaveRuntimeConfigReviewAndDefaultLibrarySettingsPropagatesInvalidReviewConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, InitConfig{}, ReviewWatchConfig{MergeMethod: "fast-forward"}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "merge_method must be merge, squash, or rebase") {
		t.Fatalf("SaveRuntimeConfigReviewAndDefaultLibrarySettings(invalid merge method) error = %v", err)
	}
}

func TestApplyReviewWatchSettingsToDocRejectsInvalidMergeMethod(t *testing.T) {
	t.Parallel()
	err := applyReviewWatchSettingsToDoc(map[string]any{}, ReviewWatchConfig{MergeMethod: "fast-forward"})
	if err == nil || !strings.Contains(err.Error(), "merge_method must be merge, squash, or rebase") {
		t.Fatalf("applyReviewWatchSettingsToDoc(invalid merge method) error = %v", err)
	}
}

func TestApplyReviewWatchSettingsToDocRejectsInvalidReviewLevel(t *testing.T) {
	t.Parallel()
	err := applyReviewWatchSettingsToDoc(map[string]any{}, ReviewWatchConfig{ReviewLevel: "extreme"})
	if err == nil || !strings.Contains(err.Error(), "review_level must be off, low, medium, or high") {
		t.Fatalf("applyReviewWatchSettingsToDoc(invalid review level) error = %v", err)
	}
}

func TestApplyReviewWatchSettingsToDocClearsDisabledFlags(t *testing.T) {
	t.Parallel()
	doc := map[string]any{"review_watch": map[string]any{"auto_merge": true, "delete_merged_branches": true}}
	if err := applyReviewWatchSettingsToDoc(doc, ReviewWatchConfig{}); err != nil {
		t.Fatalf("applyReviewWatchSettingsToDoc() error = %v", err)
	}
	// Both flags default to disabled; previously-set values must be cleared,
	// and an otherwise-empty review_watch doc is dropped entirely.
	if _, ok := doc["review_watch"]; ok {
		t.Fatalf("doc[review_watch] = %#v, want removed once it only holds defaults", doc["review_watch"])
	}
}

func TestApplyReviewWatchSettingsToDocRecordsNonSquashMergeMethod(t *testing.T) {
	t.Parallel()
	doc := map[string]any{}
	if err := applyReviewWatchSettingsToDoc(doc, ReviewWatchConfig{MergeMethod: "rebase"}); err != nil {
		t.Fatalf("applyReviewWatchSettingsToDoc() error = %v", err)
	}
	reviewDoc, ok := doc["review_watch"].(map[string]any)
	if !ok || reviewDoc["merge_method"] != "rebase" {
		t.Fatalf("doc[review_watch] = %#v, want merge_method=rebase recorded", doc["review_watch"])
	}
}

func TestReadRuntimeConfigDefaultLibraryEnabledEmptyPath(t *testing.T) {
	t.Parallel()
	if got := ReadRuntimeConfigDefaultLibraryEnabled(""); !got {
		t.Fatal("ReadRuntimeConfigDefaultLibraryEnabled(empty path) = false, want true (default)")
	}
}

func TestReadRuntimeConfigChatEnabledEmptyPath(t *testing.T) {
	t.Parallel()
	if got := ReadRuntimeConfigChatEnabled(""); got {
		t.Fatal("ReadRuntimeConfigChatEnabled(empty path) = true, want false (default)")
	}
}

func TestReadRuntimeConfigJSONEnabledEmptyPath(t *testing.T) {
	t.Parallel()
	if got := ReadRuntimeConfigJSONEnabled(""); got {
		t.Fatal("ReadRuntimeConfigJSONEnabled(empty path) = true, want false (default)")
	}
}

func TestReadRuntimeConfigStringNoKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"github_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadRuntimeConfigString(path); got != "" {
		t.Fatalf("ReadRuntimeConfigString(no keys) = %q, want empty", got)
	}
}

func TestReadRuntimeConfigStringSkipsBlankKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"github_token":"saved-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadRuntimeConfigString(path, "  ", "github_token"); got != "saved-value" {
		t.Fatalf("ReadRuntimeConfigString(blank then valid key) = %q, want saved-value", got)
	}
}

func TestReadRuntimeConfigLibraryTaskUsageEmptyPath(t *testing.T) {
	t.Parallel()
	if got := ReadRuntimeConfigLibraryTaskUsage(""); got != nil {
		t.Fatalf("ReadRuntimeConfigLibraryTaskUsage(empty path) = %v, want nil", got)
	}
}

func TestReadRuntimeConfigLibraryTaskUsageMalformedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadRuntimeConfigLibraryTaskUsage(path); got != nil {
		t.Fatalf("ReadRuntimeConfigLibraryTaskUsage(malformed) = %v, want nil", got)
	}
}

func TestIncrementRuntimeConfigLibraryTaskUsageDefaultPath(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")
	t.Chdir(t.TempDir())
	if err := IncrementRuntimeConfigLibraryTaskUsage("", InitConfig{}, "unit-test-coverage"); err != nil {
		t.Fatalf("IncrementRuntimeConfigLibraryTaskUsage(empty path) error = %v", err)
	}
	if _, err := os.Stat(runtimeConfigPath); err != nil {
		t.Fatalf("Stat(default runtime config path) error = %v, want file written", err)
	}
}

func TestIncrementRuntimeConfigLibraryTaskUsageRejectsMalformedExistingConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := IncrementRuntimeConfigLibraryTaskUsage(path, InitConfig{}, "unit-test-coverage"); err == nil {
		t.Fatal("IncrementRuntimeConfigLibraryTaskUsage(malformed config) error = nil, want load failure")
	}
}

func TestSaveRuntimeConfigGitHubTokenDefaultPath(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")
	t.Chdir(t.TempDir())
	if err := SaveRuntimeConfigGitHubToken("", InitConfig{}, "token-test-only"); err != nil {
		t.Fatalf("SaveRuntimeConfigGitHubToken(empty path) error = %v", err)
	}
	if _, err := os.Stat(runtimeConfigPath); err != nil {
		t.Fatalf("Stat(default runtime config path) error = %v, want file written", err)
	}
}

func TestSaveRuntimeConfigGitHubTokenRejectsMalformedExistingConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRuntimeConfigGitHubToken(path, InitConfig{}, "token-test-only"); err == nil {
		t.Fatal("SaveRuntimeConfigGitHubToken(malformed config) error = nil, want load failure")
	}
}

func TestWriteRuntimeConfigDocRejectsUnmarshalableValue(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	err := writeRuntimeConfigDoc(path, map[string]any{"bad": make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "encode runtime config") {
		t.Fatalf("writeRuntimeConfigDoc(unmarshalable value) error = %v, want encode failure", err)
	}
}

func TestEnsureRuntimeConfigLogLevelNilDocIsNoop(t *testing.T) {
	t.Parallel()
	// Must not panic.
	ensureRuntimeConfigLogLevel(nil, "info")
}

func TestRuntimeConfigLogLevelFromDocNilDoc(t *testing.T) {
	t.Parallel()
	if got := runtimeConfigLogLevelFromDoc(nil); got != "" {
		t.Fatalf("runtimeConfigLogLevelFromDoc(nil) = %q, want empty", got)
	}
}

func TestEnsureRuntimeConfigLogLevelPersistedEmptyPath(t *testing.T) {
	t.Parallel()
	if err := ensureRuntimeConfigLogLevelPersisted("", "info"); err != nil {
		t.Fatalf("ensureRuntimeConfigLogLevelPersisted(empty path) error = %v, want nil", err)
	}
}

func TestEnsureRuntimeConfigLogLevelPersistedRejectsMalformedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigLogLevelPersisted(path, "info"); err == nil || !strings.Contains(err.Error(), "read runtime config") {
		t.Fatalf("ensureRuntimeConfigLogLevelPersisted(malformed) error = %v, want read runtime config failure", err)
	}
}

func TestRuntimeConfigLibraryTaskUsageSkipsBlankNamesAndNonPositiveCounts(t *testing.T) {
	t.Parallel()
	got := runtimeConfigLibraryTaskUsage(map[string]any{" ": float64(5), "zero-count-task": float64(0)})
	if got != nil {
		t.Fatalf("runtimeConfigLibraryTaskUsage() = %v, want nil (blank name and non-positive count both filtered)", got)
	}
}

func TestSaveRuntimeConfigClaudeOAuthTokenDefaultPath(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")
	t.Chdir(t.TempDir())
	if err := SaveRuntimeConfigClaudeOAuthToken("", InitConfig{Version: "v1"}, "oauth-test-only"); err != nil {
		t.Fatalf("SaveRuntimeConfigClaudeOAuthToken(empty path) error = %v", err)
	}
	if _, err := os.Stat(runtimeConfigPath); err != nil {
		t.Fatalf("Stat(default runtime config path) error = %v, want file written", err)
	}
}

func TestClearRuntimeConfigClaudeOAuthTokenEmptyPath(t *testing.T) {
	t.Parallel()
	if err := ClearRuntimeConfigClaudeOAuthToken(""); err != nil {
		t.Fatalf("ClearRuntimeConfigClaudeOAuthToken(empty path) error = %v, want nil", err)
	}
}

func TestClearRuntimeConfigClaudeOAuthTokenReportsUnreadablePath(t *testing.T) {
	t.Parallel()
	// A directory at the config path makes os.ReadFile fail with an error
	// other than "not exist".
	path := t.TempDir()
	err := ClearRuntimeConfigClaudeOAuthToken(path)
	if err == nil || !strings.Contains(err.Error(), "read runtime config") {
		t.Fatalf("ClearRuntimeConfigClaudeOAuthToken(directory path) error = %v, want read runtime config failure", err)
	}
}

func TestWriteRuntimeConfigFileFailsWhenTargetIsNonEmptyDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// A non-empty directory at the destination path makes os.Rename fail,
	// exercising writeRuntimeConfigFile's replace-failure branch.
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeRuntimeConfigFile(path, []byte("{}"))
	if err == nil {
		t.Fatal("writeRuntimeConfigFile(non-empty directory target) error = nil, want rename failure")
	}
	if !strings.Contains(err.Error(), "replace runtime config") {
		t.Fatalf("writeRuntimeConfigFile() error = %v, want replace runtime config failure", err)
	}
}
