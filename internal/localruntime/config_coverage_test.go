package localruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadInitMissingFile(t *testing.T) {
	t.Parallel()
	_, err := LoadInit(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil || !strings.Contains(err.Error(), "read init config") {
		t.Fatalf("LoadInit(missing) error = %v, want read init config failure", err)
	}
}

func TestLoadInitRejectsNonObjectJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "init.json")
	if err := os.WriteFile(path, []byte(`[1,2,3]`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadInit(path)
	if err == nil || !strings.Contains(err.Error(), "parse init json") {
		t.Fatalf("LoadInit(array) error = %v, want parse init json failure", err)
	}
}

func TestLoadInitRejectsTypeMismatchedField(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "init.json")
	if err := os.WriteFile(path, []byte(`{"version":123}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadInit(path)
	if err == nil || !strings.Contains(err.Error(), "parse init json") {
		t.Fatalf("LoadInit(type mismatch) error = %v, want parse init json failure", err)
	}
}

func TestApplyDefaultsClampsMaxParallelToMinParallel(t *testing.T) {
	t.Parallel()
	cfg := InitConfig{Scheduler: SchedulerConfig{MinParallel: 5, MaxParallel: 2}}
	cfg.ApplyDefaults()
	if cfg.Scheduler.MaxParallel != 5 {
		t.Fatalf("Scheduler.MaxParallel = %d, want 5 (clamped up to MinParallel)", cfg.Scheduler.MaxParallel)
	}
	if cfg.Scheduler.MinParallel != 5 {
		t.Fatalf("Scheduler.MinParallel = %d, want 5 (unchanged)", cfg.Scheduler.MinParallel)
	}
}

func TestApplyDefaultsPreservesInvalidReviewWatchValuesForValidate(t *testing.T) {
	t.Parallel()
	cfg := InitConfig{ReviewWatch: ReviewWatchConfig{
		Writeback:   "Sideways_Mode",
		MergeMethod: "Fast_Forward",
		ReviewLevel: "Extreme_Mode",
	}}
	cfg.ApplyDefaults()
	if cfg.ReviewWatch.Writeback != "sideways-mode" {
		t.Fatalf("ReviewWatch.Writeback = %q, want lowercased-with-dashes %q", cfg.ReviewWatch.Writeback, "sideways-mode")
	}
	if cfg.ReviewWatch.MergeMethod != "fast_forward" {
		t.Fatalf("ReviewWatch.MergeMethod = %q, want lowercased %q", cfg.ReviewWatch.MergeMethod, "fast_forward")
	}
	if cfg.ReviewWatch.ReviewLevel != "extreme_mode" {
		t.Fatalf("ReviewWatch.ReviewLevel = %q, want lowercased %q", cfg.ReviewWatch.ReviewLevel, "extreme_mode")
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want rejection of the invalid preserved review_watch values")
	}
}

func TestInitConfigValidateDefaultsEmptyLogLevel(t *testing.T) {
	t.Parallel()
	cfg := validInitConfigForValidateTest()
	cfg.LogLevel = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(empty log level) error = %v, want nil (empty treated as default)", err)
	}
}
