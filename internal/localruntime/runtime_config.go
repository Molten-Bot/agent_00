package localruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
)

const (
	runtimeConfigPath            = "./.agent00/config.json"
	runtimeConfigPathEnv         = "HARNESS_RUNTIME_CONFIG_PATH"
	GitHubIssueLinkModeReference = "reference"
	GitHubIssueLinkModeClose     = "close"
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
// settings, the default-library-enabled flag, the chat-enabled flag, and the
// json-enabled flag in a single load/modify/write cycle so a mid-save failure
// cannot leave one setting persisted without the others.
// defaultLibraryEnabled, chatEnabled, and jsonEnabled are pointers so callers
// that are not changing a given setting can pass nil; the current persisted
// value is then read from the freshly loaded doc inside the same locked
// critical section, avoiding a lost-update race with a concurrent save.
func SaveRuntimeConfigReviewAndDefaultLibrarySettings(
	path string,
	initCfg InitConfig,
	reviewCfg ReviewWatchConfig,
	defaultLibraryEnabled *bool,
	chatEnabled *bool,
	jsonEnabled *bool,
) error {
	return SaveRuntimeConfigReviewDefaultLibraryAndGitHubIssuesSettings(
		path, initCfg, reviewCfg, defaultLibraryEnabled, chatEnabled, jsonEnabled, nil, nil, nil, nil,
	)
}

// SaveRuntimeConfigReviewDefaultLibraryAndGitHubIssuesSettings persists review
// automation, library, chat, JSON, and GitHub Issues settings in one update.
// githubIssueLinkMode is optional; when omitted, the existing behavior remains.
type RuntimeConfigUISettingsUpdate struct {
	OnlyAssigned         *bool
	Labels               *[]string
	DisabledRepositories *[]string
}

func SaveRuntimeConfigReviewDefaultLibraryAndGitHubIssuesSettings(
	path string,
	initCfg InitConfig,
	reviewCfg ReviewWatchConfig,
	defaultLibraryEnabled *bool,
	chatEnabled *bool,
	jsonEnabled *bool,
	githubIssuesEnabled *bool,
	githubIssueLinkMode *string,
	githubIssueAutoStartAssigned *bool,
	assignCreator *bool,
	settingsUpdates ...RuntimeConfigUISettingsUpdate,
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

	chatEnabledValue := chatEnabledFromDoc(doc)
	if chatEnabled != nil {
		chatEnabledValue = *chatEnabled
	}
	applyChatEnabledToDoc(doc, chatEnabledValue)

	jsonEnabledValue := jsonEnabledFromDoc(doc)
	if jsonEnabled != nil {
		jsonEnabledValue = *jsonEnabled
	}
	applyJSONEnabledToDoc(doc, jsonEnabledValue)
	githubIssuesEnabledValue := githubIssuesEnabledFromDoc(doc)
	if githubIssuesEnabled != nil {
		githubIssuesEnabledValue = *githubIssuesEnabled
	}
	applyGitHubIssuesEnabledToDoc(doc, githubIssuesEnabledValue)
	githubIssueLinkModeValue := githubIssueLinkModeFromDoc(doc)
	if githubIssueLinkMode != nil {
		githubIssueLinkModeValue = NormalizeGitHubIssueLinkMode(*githubIssueLinkMode)
		if githubIssueLinkModeValue == "" {
			return fmt.Errorf("github_issue_link_mode must be %q or %q", GitHubIssueLinkModeReference, GitHubIssueLinkModeClose)
		}
	}
	applyGitHubIssueLinkModeToDoc(doc, githubIssueLinkModeValue)
	autoStartAssigned := githubIssueAutoStartAssignedFromDoc(doc)
	if githubIssueAutoStartAssigned != nil {
		autoStartAssigned = *githubIssueAutoStartAssigned
	}
	applyGitHubIssueAutoStartAssignedToDoc(doc, autoStartAssigned)
	onlyAssigned := githubIssueAutoStartOnlyAssignedFromDoc(doc)
	var githubIssueAutoStartLabels *[]string
	for _, setting := range settingsUpdates {
		if setting.OnlyAssigned != nil {
			onlyAssigned = *setting.OnlyAssigned
		}
		if setting.Labels != nil {
			githubIssueAutoStartLabels = setting.Labels
		}
	}
	applyGitHubIssueAutoStartOnlyAssignedToDoc(doc, onlyAssigned)
	if githubIssueAutoStartLabels != nil {
		applyGitHubIssueAutoStartLabelsToDoc(doc, *githubIssueAutoStartLabels)
	}
	assignCreatorValue := assignCreatorFromDoc(doc)
	if assignCreator != nil {
		assignCreatorValue = *assignCreator
	}
	applyAssignCreatorToDoc(doc, assignCreatorValue)
	for _, setting := range settingsUpdates {
		if setting.DisabledRepositories != nil {
			applyDisabledRepositoriesToDoc(doc, *setting.DisabledRepositories)
		}
	}

	ensureRuntimeConfigLogLevel(doc, initCfg.LogLevel)

	return writeRuntimeConfigDoc(path, doc)
}

func ReadRuntimeConfigGitHubIssueAutoStartOnlyAssigned(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return true
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return true
	}
	return githubIssueAutoStartOnlyAssignedFromDoc(doc)
}

func githubIssueAutoStartOnlyAssignedFromDoc(doc map[string]any) bool {
	enabled, ok := doc["github_issue_auto_start_only_assigned"].(bool)
	return !ok || enabled
}

func applyGitHubIssueAutoStartOnlyAssignedToDoc(doc map[string]any, enabled bool) {
	if enabled {
		delete(doc, "github_issue_auto_start_only_assigned")
		return
	}
	doc["github_issue_auto_start_only_assigned"] = false
}

// ReadRuntimeConfigGitHubIssueAutoStartLabels returns labels that every
// automatically started issue must have.
func ReadRuntimeConfigGitHubIssueAutoStartLabels(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return nil
	}
	return githubIssueAutoStartLabelsFromDoc(doc)
}

func githubIssueAutoStartLabelsFromDoc(doc map[string]any) []string {
	raw, ok := doc["github_issue_auto_start_labels"].([]any)
	if !ok {
		return nil
	}
	return normalizeGitHubIssueAutoStartLabels(raw)
}

func normalizeGitHubIssueAutoStartLabels(values []any) []string {
	seen := make(map[string]struct{}, len(values))
	labels := make([]string, 0, len(values))
	for _, raw := range values {
		label, ok := raw.(string)
		label = strings.TrimSpace(label)
		if !ok || label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

func applyGitHubIssueAutoStartLabelsToDoc(doc map[string]any, labels []string) {
	raw := make([]any, len(labels))
	for i, label := range labels {
		raw[i] = label
	}
	normalized := normalizeGitHubIssueAutoStartLabels(raw)
	if len(normalized) == 0 {
		delete(doc, "github_issue_auto_start_labels")
		return
	}
	doc["github_issue_auto_start_labels"] = normalized
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

// RuntimeConfigUISettings holds the persisted UI and automation toggles read
// from one runtime config file.
type RuntimeConfigUISettings struct {
	DefaultLibraryEnabled            bool
	ChatEnabled                      bool
	JSONEnabled                      bool
	GitHubIssuesEnabled              bool
	GitHubIssueLinkMode              string
	GitHubIssueAutoStartAssigned     bool
	GitHubIssueAutoStartOnlyAssigned bool
	GitHubIssueAutoStartLabels       []string
	AssignCreator                    bool
	DisabledRepositories             []string
}

// ReadRuntimeConfigUISettings reads the runtime config once and returns every
// UI toggle. Each field matches its ReadRuntimeConfig* counterpart, including
// defaults when the path is empty or the file is unreadable.
func ReadRuntimeConfigUISettings(path string) RuntimeConfigUISettings {
	var doc map[string]any
	if path = strings.TrimSpace(path); path != "" {
		doc, _ = readRuntimeConfigDoc(path)
	}
	return RuntimeConfigUISettings{
		DefaultLibraryEnabled:            defaultLibraryEnabledFromDoc(doc),
		ChatEnabled:                      chatEnabledFromDoc(doc),
		JSONEnabled:                      jsonEnabledFromDoc(doc),
		GitHubIssuesEnabled:              githubIssuesEnabledFromDoc(doc),
		GitHubIssueLinkMode:              githubIssueLinkModeFromDoc(doc),
		GitHubIssueAutoStartAssigned:     githubIssueAutoStartAssignedFromDoc(doc),
		GitHubIssueAutoStartOnlyAssigned: githubIssueAutoStartOnlyAssignedFromDoc(doc),
		GitHubIssueAutoStartLabels:       githubIssueAutoStartLabelsFromDoc(doc),
		AssignCreator:                    assignCreatorFromDoc(doc),
		DisabledRepositories:             disabledRepositoriesFromDoc(doc),
	}
}

func disabledRepositoriesFromDoc(doc map[string]any) []string {
	values, _ := doc["disabled_repositories"].([]any)
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		repo, ok := value.(string)
		if !ok {
			continue
		}
		repo = strings.TrimSpace(repo)
		key := strings.ToLower(repo)
		if repo == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, repo)
	}
	return result
}

// ReadRuntimeConfigDisabledRepositories returns repositories hidden from UI
// selectors. An absent or unreadable setting means every repository is enabled.
func ReadRuntimeConfigDisabledRepositories(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return nil
	}
	return disabledRepositoriesFromDoc(doc)
}

func applyDisabledRepositoriesToDoc(doc map[string]any, repos []string) {
	values := make([]any, 0, len(repos))
	seen := map[string]struct{}{}
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		key := strings.ToLower(repo)
		if repo == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, repo)
	}
	if len(values) == 0 {
		delete(doc, "disabled_repositories")
		return
	}
	doc["disabled_repositories"] = values
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

func applyChatEnabledToDoc(doc map[string]any, enabled bool) {
	if enabled {
		doc["chat_enabled"] = true
	} else {
		delete(doc, "chat_enabled")
	}
}

// ReadRuntimeConfigChatEnabled returns the persisted chat-interface setting,
// defaulting to false (off) when unset or unreadable. The chat interface is
// experimental and must be turned on from settings.
func ReadRuntimeConfigChatEnabled(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}

	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return false
	}
	return chatEnabledFromDoc(doc)
}

// chatEnabledFromDoc returns the chat-enabled setting from an already-loaded
// runtime config doc, defaulting to false (off) when unset.
func chatEnabledFromDoc(doc map[string]any) bool {
	enabled, ok := doc["chat_enabled"].(bool)
	if !ok {
		return false
	}
	return enabled
}

func applyJSONEnabledToDoc(doc map[string]any, enabled bool) {
	if enabled {
		doc["json_enabled"] = true
	} else {
		delete(doc, "json_enabled")
	}
}

// ReadRuntimeConfigJSONEnabled returns the persisted JSON-prompt-input
// setting, defaulting to false (off) when unset or unreadable. The JSON
// input is experimental and must be turned on from settings.
func ReadRuntimeConfigJSONEnabled(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}

	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return false
	}
	return jsonEnabledFromDoc(doc)
}

// jsonEnabledFromDoc returns the json-enabled setting from an already-loaded
// runtime config doc, defaulting to false (off) when unset.
func jsonEnabledFromDoc(doc map[string]any) bool {
	enabled, ok := doc["json_enabled"].(bool)
	if !ok {
		return false
	}
	return enabled
}

func applyGitHubIssuesEnabledToDoc(doc map[string]any, enabled bool) {
	if enabled {
		doc["github_issues_enabled"] = true
	} else {
		delete(doc, "github_issues_enabled")
	}
}

// NormalizeGitHubIssueLinkMode returns the supported PR issue-link behavior,
// or an empty string when value is not supported.
func NormalizeGitHubIssueLinkMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case GitHubIssueLinkModeReference:
		return GitHubIssueLinkModeReference
	case GitHubIssueLinkModeClose:
		return GitHubIssueLinkModeClose
	default:
		return ""
	}
}

func applyGitHubIssueLinkModeToDoc(doc map[string]any, mode string) {
	if mode == GitHubIssueLinkModeClose {
		doc["github_issue_link_mode"] = GitHubIssueLinkModeClose
	} else {
		delete(doc, "github_issue_link_mode")
	}
}

// ReadRuntimeConfigGitHubIssueLinkMode reads the PR issue-link behavior,
// defaulting to reference so existing runtime configs keep issues open.
func ReadRuntimeConfigGitHubIssueLinkMode(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return GitHubIssueLinkModeReference
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return GitHubIssueLinkModeReference
	}
	return githubIssueLinkModeFromDoc(doc)
}

func githubIssueLinkModeFromDoc(doc map[string]any) string {
	mode, _ := doc["github_issue_link_mode"].(string)
	if normalized := NormalizeGitHubIssueLinkMode(mode); normalized != "" {
		return normalized
	}
	return GitHubIssueLinkModeReference
}

// ReadRuntimeConfigGitHubIssuesEnabled reports whether issue browsing is
// enabled, defaulting to off for existing and unreadable runtime configs.
func ReadRuntimeConfigGitHubIssuesEnabled(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return false
	}
	return githubIssuesEnabledFromDoc(doc)
}

// ReadRuntimeConfigGitHubIssueAutoStartAssigned reports whether newly assigned
// GitHub issues should be queued automatically by the local UI service.
func ReadRuntimeConfigGitHubIssueAutoStartAssigned(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return false
	}
	return githubIssueAutoStartAssignedFromDoc(doc)
}

func githubIssueAutoStartAssignedFromDoc(doc map[string]any) bool {
	enabled, ok := doc["github_issue_auto_start_assigned"].(bool)
	return ok && enabled
}

func applyGitHubIssueAutoStartAssignedToDoc(doc map[string]any, enabled bool) {
	if enabled {
		doc["github_issue_auto_start_assigned"] = true
		return
	}
	delete(doc, "github_issue_auto_start_assigned")
}

// ReadRuntimeConfigAssignCreator reports whether created PRs and worked issues
// are assigned to the authenticated GitHub user. It defaults on.
func ReadRuntimeConfigAssignCreator(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return true
	}
	doc, err := readRuntimeConfigDoc(path)
	if err != nil {
		return true
	}
	return assignCreatorFromDoc(doc)
}

func assignCreatorFromDoc(doc map[string]any) bool {
	enabled, ok := doc["assign_creator"].(bool)
	return !ok || enabled
}

func applyAssignCreatorToDoc(doc map[string]any, enabled bool) {
	if enabled {
		delete(doc, "assign_creator")
		return
	}
	doc["assign_creator"] = false
}

func githubIssuesEnabledFromDoc(doc map[string]any) bool {
	enabled, ok := doc["github_issues_enabled"].(bool)
	return ok && enabled
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

// ClearRuntimeConfigClaudeOAuthToken removes token overrides when switching to
// a Claude-managed login, preserving the rest of the runtime configuration.
func ClearRuntimeConfigClaudeOAuthToken(path string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read runtime config: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse runtime config: %w", err)
	}
	changed := false
	for _, key := range []string{"claude_code_oauth_token", "claudeCodeOauthToken", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, exists := doc[key]; exists {
			delete(doc, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
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
