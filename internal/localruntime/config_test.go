package localruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewWatchReviewCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level string
		want  int
	}{
		{level: "", want: 0},
		{level: "off", want: 0},
		{level: "LOW", want: 1},
		{level: "medium", want: 3},
		{level: " high ", want: 6},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			t.Parallel()
			if got := (ReviewWatchConfig{ReviewLevel: tt.level}).ReviewCount(); got != tt.want {
				t.Fatalf("ReviewCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLoadInitSupportsReviewWatchConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "init.json")
	data := `{
  "review_watch": {
    "enabled": true,
    "poll_interval_ms": 30000,
    "writeback": "off",
    "auto_merge": false,
    "delete_merged_branches": true,
    "merge_method": "rebase",
    "response_mode": "off"
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	cfg, err := LoadInit(path)
	if err != nil {
		t.Fatalf("LoadInit() error = %v", err)
	}
	if !cfg.ReviewWatch.EnabledValue() {
		t.Fatal("ReviewWatch.EnabledValue() = false, want true")
	}
	if cfg.ReviewWatch.PollIntervalMS != 30000 {
		t.Fatalf("ReviewWatch.PollIntervalMS = %d, want 30000", cfg.ReviewWatch.PollIntervalMS)
	}
	if cfg.ReviewWatch.Writeback != "off" {
		t.Fatalf("ReviewWatch.Writeback = %q, want off", cfg.ReviewWatch.Writeback)
	}
	if cfg.ReviewWatch.AutoMergeEnabled() {
		t.Fatal("ReviewWatch.AutoMergeEnabled() = true, want false")
	}
	if !cfg.ReviewWatch.DeleteMergedBranchesEnabled() {
		t.Fatal("ReviewWatch.DeleteMergedBranchesEnabled() = false, want true")
	}
	if cfg.ReviewWatch.MergeMethod != "rebase" {
		t.Fatalf("ReviewWatch.MergeMethod = %q, want rebase", cfg.ReviewWatch.MergeMethod)
	}
}

func TestLoadInitCanDisableDefaultReviewWatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "init.json")
	data := `{
  "review_watch": {
    "enabled": false
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	cfg, err := LoadInit(path)
	if err != nil {
		t.Fatalf("LoadInit() error = %v", err)
	}
	if cfg.ReviewWatch.EnabledValue() {
		t.Fatal("ReviewWatch.EnabledValue() = true, want false")
	}
}

func TestDefaultSchedulerMaxParallelForCores(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		cores int
		want  int
	}{
		{name: "non-positive falls back to adaptive minimum", cores: 0, want: 2},
		{name: "single core allows light overlap", cores: 1, want: 2},
		{name: "dual core allows one extra slot", cores: 2, want: 3},
		{name: "multi core tracks core count", cores: 8, want: 8},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultSchedulerMaxParallelForCores(tc.cores); got != tc.want {
				t.Fatalf("defaultSchedulerMaxParallelForCores(%d) = %d, want %d", tc.cores, got, tc.want)
			}
		})
	}
}

func TestLoadInitRejectsUnknownLogLevel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "init.json")
	data := `{
  "agent_token": "agent_live_token",
  "log_level": "chatty"
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	if _, err := LoadInit(path); err == nil {
		t.Fatal("LoadInit() error = nil, want invalid log_level error")
	} else if !strings.Contains(err.Error(), "log_level must be one of error, warn, info, debug") {
		t.Fatalf("LoadInit() error = %v", err)
	}
}

func TestLoadInitSupportsAgentHarnessAndCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "init.json")
	data := `{
  "agent_harness": "CLAUDE",
  "agent_command": "claude-custom"
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	cfg, err := LoadInit(path)
	if err != nil {
		t.Fatalf("LoadInit() error = %v", err)
	}
	if got, want := cfg.AgentHarness, "claude"; got != want {
		t.Fatalf("AgentHarness = %q, want %q", got, want)
	}
	if got, want := cfg.AgentCommand, "claude-custom"; got != want {
		t.Fatalf("AgentCommand = %q, want %q", got, want)
	}
}

func validInitConfigForValidateTest() InitConfig {
	return InitConfig{
		Version:      "v1",
		LogLevel:     "info",
		AgentHarness: "codex",
		Scheduler: SchedulerConfig{
			MaxParallel:            2,
			MinParallel:            1,
			SampleWindow:           5,
			SampleIntervalMS:       1500,
			CPUHighWatermark:       85,
			MemoryHighWatermark:    90,
			DiskIOHighWatermarkMBs: 120,
		},
		ReviewWatch: ReviewWatchConfig{
			PollIntervalMS: 60000,
			Writeback:      "summary-comment",
			MergeMethod:    "squash",
			ReviewLevel:    "off",
		},
	}
}

func TestInitConfigValidateAcceptsWellFormedConfig(t *testing.T) {
	t.Parallel()

	if err := validInitConfigForValidateTest().Validate(); err != nil {
		t.Fatalf("Validate(valid config) error = %v, want nil", err)
	}
}

func TestInitConfigValidateRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*InitConfig)
		wantErr string
	}{
		{"empty version", func(c *InitConfig) { c.Version = "" }, "version is required"},
		{"unsupported version", func(c *InitConfig) { c.Version = "v2" }, `unsupported version "v2"`},
		{"unknown log level", func(c *InitConfig) { c.LogLevel = "chatty" }, "log_level must be one of"},
		{"unknown agent harness", func(c *InitConfig) { c.AgentHarness = "bogus" }, "unsupported agentHarness"},
		{"max_parallel below minimum", func(c *InitConfig) { c.Scheduler.MaxParallel = 0 }, "scheduler.max_parallel must be >= 1"},
		{"min_parallel below minimum", func(c *InitConfig) { c.Scheduler.MinParallel = 0 }, "scheduler.min_parallel must be >= 1"},
		{"min_parallel exceeds max_parallel", func(c *InitConfig) {
			c.Scheduler.MinParallel = 3
			c.Scheduler.MaxParallel = 2
		}, "scheduler.min_parallel must be <= scheduler.max_parallel"},
		{"sample_window below minimum", func(c *InitConfig) { c.Scheduler.SampleWindow = 0 }, "scheduler.sample_window must be >= 1"},
		{"sample_interval_ms below minimum", func(c *InitConfig) { c.Scheduler.SampleIntervalMS = 100 }, "scheduler.sample_interval_ms must be >= 250"},
		{"cpu_high_watermark zero", func(c *InitConfig) { c.Scheduler.CPUHighWatermark = 0 }, "scheduler.cpu_high_watermark must be > 0 and <= 100"},
		{"cpu_high_watermark above 100", func(c *InitConfig) { c.Scheduler.CPUHighWatermark = 101 }, "scheduler.cpu_high_watermark must be > 0 and <= 100"},
		{"memory_high_watermark zero", func(c *InitConfig) { c.Scheduler.MemoryHighWatermark = 0 }, "scheduler.memory_high_watermark must be > 0 and <= 100"},
		{"memory_high_watermark above 100", func(c *InitConfig) { c.Scheduler.MemoryHighWatermark = 150 }, "scheduler.memory_high_watermark must be > 0 and <= 100"},
		{"disk_io_high_watermark non-positive", func(c *InitConfig) { c.Scheduler.DiskIOHighWatermarkMBs = 0 }, "scheduler.disk_io_high_watermark_mb_s must be > 0"},
		{"review poll interval too low while enabled", func(c *InitConfig) { c.ReviewWatch.PollIntervalMS = 1000 }, "review_watch.poll_interval_ms must be >= 5000"},
		{"review writeback invalid", func(c *InitConfig) { c.ReviewWatch.Writeback = "sideways" }, "review_watch.writeback must be summary-comment or off"},
		{"review merge method invalid", func(c *InitConfig) { c.ReviewWatch.MergeMethod = "fast-forward" }, "review_watch.merge_method must be merge, squash, or rebase"},
		{"review level invalid", func(c *InitConfig) { c.ReviewWatch.ReviewLevel = "extreme" }, "review_watch.review_level must be off, low, medium, or high"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validInitConfigForValidateTest()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadInitReadsAgentRuntimeFromEnv(t *testing.T) {
	t.Setenv("HARNESS_AGENT_HARNESS", "CLAUDE")
	t.Setenv("HARNESS_AGENT_COMMAND", "claude-custom")

	dir := t.TempDir()
	path := filepath.Join(dir, "init.json")
	data := `{
  "agent_token": "token"
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	cfg, err := LoadInit(path)
	if err != nil {
		t.Fatalf("LoadInit() error = %v", err)
	}
	if got, want := cfg.AgentHarness, "claude"; got != want {
		t.Fatalf("AgentHarness = %q, want %q", got, want)
	}
	if got, want := cfg.AgentCommand, "claude-custom"; got != want {
		t.Fatalf("AgentCommand = %q, want %q", got, want)
	}
}
