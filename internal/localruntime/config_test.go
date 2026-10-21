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
