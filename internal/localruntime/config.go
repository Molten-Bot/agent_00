package localruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
)

const defaultInitVersion = "v1"

// InitConfig is the init.json contract for local server mode.
type InitConfig struct {
	Version           string            `json:"version"`
	LogLevel          string            `json:"log_level,omitempty"`
	AgentHarness      string            `json:"agent_harness,omitempty"`
	AgentCommand      string            `json:"agent_command,omitempty"`
	GitHubToken       string            `json:"github_token,omitempty"`
	OpenAIAPIKey      string            `json:"openai_api_key,omitempty"`
	RuntimeConfigPath string            `json:"-"`
	Dispatcher        DispatcherConfig  `json:"dispatcher"`
	ReviewWatch       ReviewWatchConfig `json:"review_watch,omitempty"`
}

// DispatcherConfig controls local worker behavior.
type DispatcherConfig struct {
	MaxParallel            int     `json:"max_parallel"`
	MinParallel            int     `json:"min_parallel"`
	SampleWindow           int     `json:"sample_window"`
	SampleIntervalMS       int     `json:"sample_interval_ms"`
	CPUHighWatermark       float64 `json:"cpu_high_watermark"`
	MemoryHighWatermark    float64 `json:"memory_high_watermark"`
	DiskIOHighWatermarkMBs float64 `json:"disk_io_high_watermark_mb_s"`
}

// ReviewWatchConfig controls GitHub review watching and post-task review automation.
type ReviewWatchConfig struct {
	Enabled              *bool  `json:"enabled,omitempty"`
	PollIntervalMS       int    `json:"poll_interval_ms,omitempty"`
	Writeback            string `json:"writeback,omitempty"`
	ReviewLevel          string `json:"review_level,omitempty"`
	AutoMerge            *bool  `json:"auto_merge,omitempty"`
	DeleteMergedBranches *bool  `json:"delete_merged_branches,omitempty"`
	MergeMethod          string `json:"merge_method,omitempty"`
	ResponseMode         string `json:"response_mode,omitempty"`
}

// EnabledValue returns the effective local review watcher setting. The watcher
// defaults on so a GitHub-authenticated runtime can respond to review requests
// without extra config.
func (c ReviewWatchConfig) EnabledValue() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// AutoMergeEnabled returns the effective clean-review merge setting.
func (c ReviewWatchConfig) AutoMergeEnabled() bool {
	if c.AutoMerge == nil {
		return false
	}
	return *c.AutoMerge
}

// ReviewCount returns the maximum number of post-task review passes configured
// by the review level.
func (c ReviewWatchConfig) ReviewCount() int {
	switch NormalizeReviewLevel(c.ReviewLevel) {
	case "low":
		return 1
	case "medium":
		return 3
	case "high":
		return 6
	default:
		return 0
	}
}

// DeleteMergedBranchesEnabled returns the effective merged-branch cleanup setting.
func (c ReviewWatchConfig) DeleteMergedBranchesEnabled() bool {
	if c.DeleteMergedBranches == nil {
		return false
	}
	return *c.DeleteMergedBranches
}

// LoadInit reads and validates JSON/JSONC init config.
func LoadInit(path string) (InitConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return InitConfig{}, fmt.Errorf("read init config: %w", err)
	}

	cleaned := stripLineComments(data)
	var cfg InitConfig
	dec := json.NewDecoder(bytes.NewReader(cleaned))
	if err := dec.Decode(&cfg); err != nil {
		return InitConfig{}, fmt.Errorf("parse init json: %w", err)
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return InitConfig{}, err
	}
	return cfg, nil
}

// ApplyDefaults normalizes and fills optional values.
func (c *InitConfig) ApplyDefaults() {
	c.Version = strings.TrimSpace(c.Version)
	if c.Version == "" {
		c.Version = defaultInitVersion
	}

	rawLogLevel := strings.TrimSpace(c.LogLevel)
	switch {
	case rawLogLevel == "":
		c.LogLevel = DefaultLogLevel
	case NormalizeLogLevel(rawLogLevel) != "":
		c.LogLevel = NormalizeLogLevel(rawLogLevel)
	default:
		// Preserve invalid value so Validate can return a clear error.
		c.LogLevel = strings.ToLower(rawLogLevel)
	}
	c.GitHubToken = strings.TrimSpace(c.GitHubToken)
	c.OpenAIAPIKey = strings.TrimSpace(c.OpenAIAPIKey)
	c.AgentHarness = strings.ToLower(strings.TrimSpace(c.AgentHarness))
	if c.AgentHarness == "" {
		c.AgentHarness = strings.ToLower(strings.TrimSpace(os.Getenv("HARNESS_AGENT_HARNESS")))
	}
	c.AgentCommand = strings.TrimSpace(c.AgentCommand)
	if c.AgentCommand == "" {
		c.AgentCommand = strings.TrimSpace(os.Getenv("HARNESS_AGENT_COMMAND"))
	}
	if c.Dispatcher.MinParallel < 1 {
		c.Dispatcher.MinParallel = 1
	}
	if c.Dispatcher.MaxParallel < 1 {
		c.Dispatcher.MaxParallel = defaultDispatcherMaxParallel()
	}
	if c.Dispatcher.MaxParallel < c.Dispatcher.MinParallel {
		c.Dispatcher.MaxParallel = c.Dispatcher.MinParallel
	}
	if c.Dispatcher.SampleWindow < 1 {
		c.Dispatcher.SampleWindow = 5
	}
	if c.Dispatcher.SampleIntervalMS < 250 {
		c.Dispatcher.SampleIntervalMS = 1500
	}
	if c.Dispatcher.CPUHighWatermark <= 0 {
		c.Dispatcher.CPUHighWatermark = 85
	}
	if c.Dispatcher.MemoryHighWatermark <= 0 {
		c.Dispatcher.MemoryHighWatermark = 90
	}
	if c.Dispatcher.DiskIOHighWatermarkMBs <= 0 {
		c.Dispatcher.DiskIOHighWatermarkMBs = 120
	}

	rawReviewWriteback := strings.TrimSpace(c.ReviewWatch.Writeback)
	rawReviewMergeMethod := strings.TrimSpace(c.ReviewWatch.MergeMethod)
	rawReviewLevel := strings.TrimSpace(c.ReviewWatch.ReviewLevel)
	if normalized := normalizeReviewWatchWriteback(rawReviewWriteback); normalized != "" || rawReviewWriteback == "" {
		c.ReviewWatch.Writeback = normalized
	} else {
		c.ReviewWatch.Writeback = strings.ReplaceAll(strings.ToLower(rawReviewWriteback), "_", "-")
	}
	if normalized := normalizeReviewWatchMergeMethod(rawReviewMergeMethod); normalized != "" || rawReviewMergeMethod == "" {
		c.ReviewWatch.MergeMethod = normalized
	} else {
		c.ReviewWatch.MergeMethod = strings.ToLower(rawReviewMergeMethod)
	}
	if normalized := NormalizeReviewLevel(rawReviewLevel); normalized != "" || rawReviewLevel == "" {
		c.ReviewWatch.ReviewLevel = normalized
	} else {
		c.ReviewWatch.ReviewLevel = strings.ToLower(rawReviewLevel)
	}
	c.ReviewWatch.ResponseMode = strings.TrimSpace(c.ReviewWatch.ResponseMode)
	if c.ReviewWatch.PollIntervalMS <= 0 {
		c.ReviewWatch.PollIntervalMS = 60000
	}
	if c.ReviewWatch.Writeback == "" {
		c.ReviewWatch.Writeback = "summary-comment"
	}
	if c.ReviewWatch.MergeMethod == "" {
		c.ReviewWatch.MergeMethod = "squash"
	}
	if c.ReviewWatch.ReviewLevel == "" {
		c.ReviewWatch.ReviewLevel = "off"
	}
	if c.ReviewWatch.ResponseMode == "" {
		c.ReviewWatch.ResponseMode = "off"
	}
}

// Validate checks required values.
func (c InitConfig) Validate() error {
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("version is required")
	}
	if c.Version != "v1" {
		return fmt.Errorf("unsupported version %q", c.Version)
	}
	logLevel := strings.TrimSpace(c.LogLevel)
	if logLevel == "" {
		logLevel = DefaultLogLevel
	}
	if _, err := ParseLogLevel(logLevel); err != nil {
		return err
	}
	if _, err := agentruntime.Resolve(c.AgentHarness, c.AgentCommand); err != nil {
		return err
	}
	if c.Dispatcher.MaxParallel < 1 {
		return fmt.Errorf("dispatcher.max_parallel must be >= 1")
	}
	if c.Dispatcher.MinParallel < 1 {
		return fmt.Errorf("dispatcher.min_parallel must be >= 1")
	}
	if c.Dispatcher.MinParallel > c.Dispatcher.MaxParallel {
		return fmt.Errorf("dispatcher.min_parallel must be <= dispatcher.max_parallel")
	}
	if c.Dispatcher.SampleWindow < 1 {
		return fmt.Errorf("dispatcher.sample_window must be >= 1")
	}
	if c.Dispatcher.SampleIntervalMS < 250 {
		return fmt.Errorf("dispatcher.sample_interval_ms must be >= 250")
	}
	if c.Dispatcher.CPUHighWatermark <= 0 || c.Dispatcher.CPUHighWatermark > 100 {
		return fmt.Errorf("dispatcher.cpu_high_watermark must be > 0 and <= 100")
	}
	if c.Dispatcher.MemoryHighWatermark <= 0 || c.Dispatcher.MemoryHighWatermark > 100 {
		return fmt.Errorf("dispatcher.memory_high_watermark must be > 0 and <= 100")
	}
	if c.Dispatcher.DiskIOHighWatermarkMBs <= 0 {
		return fmt.Errorf("dispatcher.disk_io_high_watermark_mb_s must be > 0")
	}
	if c.ReviewWatch.EnabledValue() && c.ReviewWatch.PollIntervalMS < 5000 {
		return fmt.Errorf("review_watch.poll_interval_ms must be >= 5000")
	}
	if c.ReviewWatch.Writeback != "" && normalizeReviewWatchWriteback(c.ReviewWatch.Writeback) == "" {
		return fmt.Errorf("review_watch.writeback must be summary-comment or off")
	}
	if c.ReviewWatch.MergeMethod != "" && normalizeReviewWatchMergeMethod(c.ReviewWatch.MergeMethod) == "" {
		return fmt.Errorf("review_watch.merge_method must be merge, squash, or rebase")
	}
	if strings.TrimSpace(c.ReviewWatch.ReviewLevel) != "" && NormalizeReviewLevel(c.ReviewWatch.ReviewLevel) == "" {
		return fmt.Errorf("review_watch.review_level must be off, low, medium, or high")
	}
	return nil
}

// NormalizeReviewLevel validates and canonicalizes the post-task review level.
func NormalizeReviewLevel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizeReviewWatchWriteback(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "":
		return ""
	case "summary", "comment", "summary-comment":
		return "summary-comment"
	case "off", "none", "disabled":
		return "off"
	default:
		return ""
	}
}

func normalizeReviewWatchMergeMethod(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case "merge", "squash", "rebase":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func defaultDispatcherMaxParallel() int {
	return defaultDispatcherMaxParallelForCores(runtime.NumCPU())
}

func defaultDispatcherMaxParallelForCores(cores int) int {
	// A single reported CPU often still has enough headroom for at least two
	// concurrent harness jobs because work is frequently I/O-bound.
	switch {
	case cores <= 1:
		return 2
	case cores == 2:
		return 3
	default:
		return cores
	}
}

func stripLineComments(data []byte) []byte {
	var out []byte
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		ch := data[i]

		if inString {
			out = append(out, ch)
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}

		if ch == '/' && i+1 < len(data) && data[i+1] == '/' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) && data[i] == '\n' {
				out = append(out, '\n')
			}
			continue
		}

		out = append(out, ch)
	}

	return out
}
