package localruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
)

const (
	runtimeConfigPath    = "./.agent00/config.json"
	runtimeConfigPathEnv = "HARNESS_RUNTIME_CONFIG_PATH"
)

// Serialize read/modify/write operations across settings and recurring runs.
var runtimeConfigMu sync.Mutex

// RuntimeConfig persists local agent and review preferences.
type RuntimeConfig struct {
	InitConfig

	LibraryTaskUsage map[string]int             `json:"library_task_usage,omitempty"`
	LibrarySchedules map[string]LibrarySchedule `json:"library_schedules,omitempty"`
}

// ApplyDefaults normalizes a persisted runtime config.
func (c *RuntimeConfig) ApplyDefaults() {
	c.InitConfig.ApplyDefaults()

}

// Validate checks required values for local startup from config.json.
func (c RuntimeConfig) Validate() error {
	if err := c.InitConfig.Validate(); err != nil {
		return err
	}
	return nil
}

func (c RuntimeConfig) Init() InitConfig {
	initCfg := c.InitConfig
	initCfg.RuntimeConfigPath = c.RuntimeConfigPath
	return initCfg
}

// LoadRuntimeConfig reads and validates a persisted runtime config JSON file.
func LoadRuntimeConfig(path string) (RuntimeConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultRuntimeConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read runtime config: %w", err)
	}

	var cfg RuntimeConfig
	data, err = normalizeSchedulerJSON(data)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse runtime config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse runtime config: %w", err)
	}

	cfg.RuntimeConfigPath = path
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return RuntimeConfig{}, err
	}
	_ = ensureRuntimeConfigLogLevelPersisted(path, cfg.LogLevel)

	return cfg, nil
}

// SaveRuntimeConfigGitHubToken persists github_token to the runtime config JSON
// while preserving other configuration fields.
func SaveRuntimeConfigGitHubToken(path string, initCfg InitConfig, gitHubToken string) error {
	return saveRuntimeConfigStringField(
		path,
		initCfg,
		gitHubToken,
		"github token is required",
		"github_token",
	)
}

// SaveRuntimeConfigAgentRuntime persists agent_harness/agent_command to the
// runtime config while preserving other configuration fields. Once configured,
// switching to a different harness or command through this API is rejected.
func SaveRuntimeConfigAgentRuntime(path string, initCfg InitConfig, harness, command string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultRuntimeConfigPath()
	}

	runtime, err := agentruntime.Resolve(harness, command)
	if err != nil {
		return err
	}

	doc, err := loadRuntimeConfigDoc(path, initCfg)
	if err != nil {
		return err
	}

	existingHarness := strings.ToLower(docStringValue(doc["agent_harness"]))
	if existingHarness == "" {
		existingHarness = strings.ToLower(docStringValue(doc["agentHarness"]))
	}
	existingCommand := docStringValue(doc["agent_command"])
	if existingCommand == "" {
		existingCommand = docStringValue(doc["agentCommand"])
	}

	if existingHarness != "" && existingHarness != runtime.Harness {
		return fmt.Errorf("agent harness is already configured as %q", existingHarness)
	}
	if existingHarness != "" && existingCommand != "" && existingCommand != runtime.Command {
		return fmt.Errorf(
			"agent command is already configured as %q for harness %q",
			existingCommand,
			existingHarness,
		)
	}

	doc["agent_harness"] = runtime.Harness
	doc["agent_command"] = runtime.Command
	ensureRuntimeConfigLogLevel(doc, initCfg.LogLevel)

	return writeRuntimeConfigDoc(path, doc)
}

// SaveRuntimeConfigReviewAndDefaultLibrarySettings persists review automation
// settings and the default-library-enabled flag in a single load/modify/write
// cycle so a mid-save failure cannot leave one setting persisted without the
// other. defaultLibraryEnabled is a pointer so callers that are not changing
// the setting can pass nil; the current persisted value is then read from the
// freshly loaded doc inside the same locked critical section, avoiding a
// lost-update race with a concurrent save.
func SaveRuntimeConfigReviewAndDefaultLibrarySettings(
	path string,
	initCfg InitConfig,
	reviewCfg ReviewWatchConfig,
	defaultLibraryEnabled *bool,
) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultRuntimeConfigPath()
	}

	doc, err := loadRuntimeConfigDoc(path, initCfg)
	if err != nil {
		return err
	}
	if err := applyReviewWatchSettingsToDoc(doc, reviewCfg); err != nil {
		return err
	}
	enabled := defaultLibraryEnabledFromDoc(doc)
	if defaultLibraryEnabled != nil {
		enabled = *defaultLibraryEnabled
	}
	applyDefaultLibraryEnabledToDoc(doc, enabled)
	ensureRuntimeConfigLogLevel(doc, initCfg.LogLevel)

	return writeRuntimeConfigDoc(path, doc)
}

func applyReviewWatchSettingsToDoc(doc map[string]any, reviewCfg ReviewWatchConfig) error {
	rawMergeMethod := strings.TrimSpace(reviewCfg.MergeMethod)
	reviewCfg.MergeMethod = normalizeReviewWatchMergeMethod(rawMergeMethod)
	if rawMergeMethod != "" && reviewCfg.MergeMethod == "" {
		return fmt.Errorf("review_watch.merge_method must be merge, squash, or rebase")
	}
	if reviewCfg.MergeMethod == "" {
		reviewCfg.MergeMethod = "squash"
	}
	rawReviewLevel := strings.TrimSpace(reviewCfg.ReviewLevel)
	reviewCfg.ReviewLevel = NormalizeReviewLevel(rawReviewLevel)
	if rawReviewLevel != "" && reviewCfg.ReviewLevel == "" {
		return fmt.Errorf("review_watch.review_level must be off, low, medium, or high")
	}
	if reviewCfg.ReviewLevel == "" {
		reviewCfg.ReviewLevel = "off"
	}

	reviewDoc := map[string]any{}
	if existing, ok := doc["review_watch"].(map[string]any); ok {
		for key, value := range existing {
			reviewDoc[key] = value
		}
	}

	if reviewCfg.AutoMergeEnabled() {
		reviewDoc["auto_merge"] = true
	} else {
		delete(reviewDoc, "auto_merge")
	}

	if reviewCfg.DeleteMergedBranchesEnabled() {
		reviewDoc["delete_merged_branches"] = true
	} else {
		delete(reviewDoc, "delete_merged_branches")
	}

	switch reviewCfg.ReviewLevel {
	case "", "off":
		delete(reviewDoc, "review_level")
	default:
		reviewDoc["review_level"] = reviewCfg.ReviewLevel
	}

	switch normalizeReviewWatchMergeMethod(reviewCfg.MergeMethod) {
	case "", "squash":
		delete(reviewDoc, "merge_method")
	default:
		reviewDoc["merge_method"] = normalizeReviewWatchMergeMethod(reviewCfg.MergeMethod)
	}

	if len(reviewDoc) == 0 || runtimeConfigReviewDocOnlyDefaults(reviewDoc) {
		delete(doc, "review_watch")
	} else {
		doc["review_watch"] = reviewDoc
	}
	return nil
}

func runtimeConfigReviewDocOnlyDefaults(doc map[string]any) bool {
	if len(doc) == 0 {
		return true
	}
	for key, value := range doc {
		switch key {
		case "poll_interval_ms":
			count, ok := runtimeConfigLibraryTaskUsageCount(value)
			if !ok || count != 60000 {
				return false
			}
		case "writeback":
			if docStringValue(value) != "summary-comment" {
				return false
			}
		case "response_mode":
			if docStringValue(value) != "off" {
				return false
			}
		case "review_level":
			if NormalizeReviewLevel(docStringValue(value)) != "off" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func applyDefaultLibraryEnabledToDoc(doc map[string]any, enabled bool) {
	if enabled {
		delete(doc, "default_library_enabled")
	} else {
		doc["default_library_enabled"] = false
	}
}

// ReadRuntimeConfigDefaultLibraryEnabled returns the persisted default
// library setting, defaulting to true (shown) when unset or unreadable.
func ReadRuntimeConfigDefaultLibraryEnabled(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return true
	}

	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return true
	}
	return defaultLibraryEnabledFromDoc(doc)
}

// defaultLibraryEnabledFromDoc returns the default-library-enabled setting
// from an already-loaded runtime config doc, defaulting to true (shown) when
// unset.
func defaultLibraryEnabledFromDoc(doc map[string]any) bool {
	enabled, ok := doc["default_library_enabled"].(bool)
	if !ok {
		return true
	}
	return enabled
}

// ReadRuntimeConfigString returns the first non-empty string for the provided
// keys from a runtime config file. It returns an empty string when the file
// cannot be read or parsed.
func ReadRuntimeConfigString(path string, keys ...string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if len(keys) == 0 {
		return ""
	}

	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return ""
	}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if value := docStringValue(doc[key]); value != "" {
			return value
		}
	}
	return ""
}

// ReadRuntimeConfigLibraryTaskUsage returns normalized per-task usage counts
// from the persisted runtime config. Invalid or non-positive entries are
// ignored.
func ReadRuntimeConfigLibraryTaskUsage(path string) map[string]int {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}

	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return nil
	}
	return runtimeConfigLibraryTaskUsage(doc["library_task_usage"])
}

// IncrementRuntimeConfigLibraryTaskUsage records one successful local selection
// of a library task in config.json while preserving unrelated runtime config
// settings already stored on disk.
func IncrementRuntimeConfigLibraryTaskUsage(path string, initCfg InitConfig, taskName string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultRuntimeConfigPath()
	}

	taskName = strings.TrimSpace(taskName)
	if taskName == "" {
		return fmt.Errorf("library task name is required")
	}

	doc, err := loadRuntimeConfigDoc(path, initCfg)
	if err != nil {
		return err
	}

	usage := runtimeConfigLibraryTaskUsage(doc["library_task_usage"])
	if usage == nil {
		usage = map[string]int{}
	}
	usage[taskName]++
	doc["library_task_usage"] = usage
	ensureRuntimeConfigLogLevel(doc, initCfg.LogLevel)

	return writeRuntimeConfigDoc(path, doc)
}

func saveRuntimeConfigStringField(
	path string,
	initCfg InitConfig,
	value string,
	emptyErr string,
	field string,
) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultRuntimeConfigPath()
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New(emptyErr)
	}

	doc, err := loadRuntimeConfigDoc(path, initCfg)
	if err != nil {
		return err
	}
	doc[field] = value
	ensureRuntimeConfigLogLevel(doc, initCfg.LogLevel)

	return writeRuntimeConfigDoc(path, doc)
}

func writeRuntimeConfigDoc(path string, doc map[string]any) error {
	if _, exists := doc["scheduler"]; !exists {
		if value, ok := doc["dispatcher"]; ok {
			doc["scheduler"] = value
		}
	}
	delete(doc, "dispatcher")
	// Discard retired remote-service preferences when saving local settings.
	for _, key := range []string{
		"base_url", "baseUrl", "bind_token", "bindToken", "agent_token", "agentToken",
		"token", "session_key", "sessionKey", "timeout_ms", "timeoutMs",
		"handle", "profile", "profile_markdown", "display_name", "emoji",
	} {
		delete(doc, key)
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime config: %w", err)
	}
	encoded = append(encoded, '\n')
	return writeRuntimeConfigFile(path, encoded)
}

func loadRuntimeConfigDoc(path string, initCfg InitConfig) (map[string]any, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		doc := map[string]any{}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse runtime config: %w", err)
		}
		return doc, nil
	case errors.Is(err, os.ErrNotExist):
		baseDoc, buildErr := runtimeConfigBaseDoc(initCfg)
		if buildErr != nil {
			return nil, buildErr
		}
		return baseDoc, nil
	default:
		return nil, fmt.Errorf("read runtime config: %w", err)
	}
}

func readRuntimeConfigDoc(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func docStringValue(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func ensureRuntimeConfigLogLevel(doc map[string]any, preferred string) {
	if doc == nil {
		return
	}
	if existing := runtimeConfigLogLevelFromDoc(doc); existing != "" {
		doc["log_level"] = existing
		return
	}
	level := NormalizeLogLevel(preferred)
	if level == "" {
		level = DefaultLogLevel
	}
	doc["log_level"] = level
}

func runtimeConfigLogLevelFromDoc(doc map[string]any) string {
	if doc == nil {
		return ""
	}
	for _, key := range []string{"log_level", "logLevel", "LOG_LEVEL"} {
		if level := NormalizeLogLevel(docStringValue(doc[key])); level != "" {
			return level
		}
	}
	return ""
}

func ensureRuntimeConfigLogLevelPersisted(path, preferred string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return fmt.Errorf("read runtime config: %w", err)
	}
	if runtimeConfigLogLevelFromDoc(doc) != "" {
		return nil
	}
	ensureRuntimeConfigLogLevel(doc, preferred)
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime config: %w", err)
	}
	encoded = append(encoded, '\n')
	return writeRuntimeConfigFile(path, encoded)
}

func runtimeConfigLibraryTaskUsage(v any) map[string]int {
	doc, ok := v.(map[string]any)
	if !ok || len(doc) == 0 {
		return nil
	}

	usage := make(map[string]int, len(doc))
	for rawName, rawCount := range doc {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		count, ok := runtimeConfigLibraryTaskUsageCount(rawCount)
		if !ok || count <= 0 {
			continue
		}
		usage[name] = count
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

func runtimeConfigLibraryTaskUsageCount(v any) (int, bool) {
	switch typed := v.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	default:
		return 0, false
	}
}

// SaveRuntimeConfigClaudeOAuthToken persists claude_code_oauth_token to the
// runtime config JSON while preserving other configuration fields.
func SaveRuntimeConfigClaudeOAuthToken(path string, initCfg InitConfig, oauthToken string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultRuntimeConfigPath()
	}

	oauthToken = strings.TrimSpace(oauthToken)
	if oauthToken == "" {
		return fmt.Errorf("claude oauth token is required")
	}

	doc := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse runtime config: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		baseDoc, buildErr := runtimeConfigBaseDoc(initCfg)
		if buildErr != nil {
			return buildErr
		}
		doc = baseDoc
	default:
		return fmt.Errorf("read runtime config: %w", err)
	}

	doc["claude_code_oauth_token"] = oauthToken
	ensureRuntimeConfigLogLevel(doc, initCfg.LogLevel)
	return writeRuntimeConfigDoc(path, doc)
}

func runtimeConfigBaseDoc(initCfg InitConfig) (map[string]any, error) {
	explicitHarness := strings.TrimSpace(initCfg.AgentHarness)
	explicitCommand := strings.TrimSpace(initCfg.AgentCommand)
	initCfg.ApplyDefaults()
	if explicitHarness == "" {
		initCfg.AgentHarness = ""
	}
	if explicitCommand == "" {
		initCfg.AgentCommand = ""
	}
	encoded, err := json.Marshal(initCfg)
	if err != nil {
		return nil, fmt.Errorf("encode runtime config base: %w", err)
	}

	doc := map[string]any{}
	if err := json.Unmarshal(encoded, &doc); err != nil {
		return nil, fmt.Errorf("decode runtime config base: %w", err)
	}
	return doc, nil
}

func writeRuntimeConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create runtime config dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp runtime config: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp runtime config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp runtime config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp runtime config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace runtime config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod runtime config: %w", err)
	}
	return nil
}

func defaultRuntimeConfigPath() string {
	if path := strings.TrimSpace(os.Getenv(runtimeConfigPathEnv)); path != "" {
		return path
	}
	return runtimeConfigPath
}

func ResolveRuntimeConfigPath(initPath string) string {
	if path := strings.TrimSpace(os.Getenv(runtimeConfigPathEnv)); path != "" {
		return path
	}

	initPath = strings.TrimSpace(initPath)
	if initPath == "" {
		return runtimeConfigPath
	}

	return filepath.Join(filepath.Dir(initPath), "config.json")
}
