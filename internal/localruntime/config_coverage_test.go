package localruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripLineCommentsStripsTrailingLineComment(t *testing.T) {
	t.Parallel()
	got := stripLineComments([]byte("{\"a\":1} // trailing comment\n\"b\"\n"))
	want := "{\"a\":1} \n\"b\"\n"
	if string(got) != want {
		t.Fatalf("stripLineComments() = %q, want %q", string(got), want)
	}
}

func TestStripLineCommentsUnterminatedCommentAtEOF(t *testing.T) {
	t.Parallel()
	// A trailing "//" comment with no newline runs to EOF; nothing after it
	// should survive, and no trailing newline should be invented.
	got := stripLineComments([]byte(`{"a":1} // no newline after this`))
	want := `{"a":1} `
	if string(got) != want {
		t.Fatalf("stripLineComments() = %q, want %q", string(got), want)
	}
}

func TestStripLineCommentsIgnoresSlashesInsideStrings(t *testing.T) {
	t.Parallel()
	got := stripLineComments([]byte(`{"url":"https://example.com"}`))
	want := `{"url":"https://example.com"}`
	if string(got) != want {
		t.Fatalf("stripLineComments() = %q, want %q (// inside a string must not be treated as a comment)", string(got), want)
	}
}

func TestStripLineCommentsHandlesEscapedBackslashBeforeQuote(t *testing.T) {
	t.Parallel()
	// The string contains an escaped backslash immediately followed by the
	// closing quote: "x\\". A naive scanner could mistake the second
	// backslash for the start of an escape sequence and fail to notice the
	// string has closed.
	got := stripLineComments([]byte(`{"a":"x\\"} // comment`))
	want := `{"a":"x\\"} `
	if string(got) != want {
		t.Fatalf("stripLineComments() = %q, want %q", string(got), want)
	}
}

func TestStripLineCommentsHandlesEscapedCharacterInsideString(t *testing.T) {
	t.Parallel()
	// A single backslash escaping a non-quote character (e.g. \n) inside a
	// string: the escape must be consumed without ending the string, so a
	// "//" appearing later in the same string is still not a comment.
	got := stripLineComments([]byte(`{"a":"line\n// not a comment"}`))
	want := `{"a":"line\n// not a comment"}`
	if string(got) != want {
		t.Fatalf("stripLineComments() = %q, want %q", string(got), want)
	}
}

func TestStripLineCommentsDoesNotSupportBlockComments(t *testing.T) {
	t.Parallel()
	// This implementation only strips "//" line comments; a "/* */" block
	// comment is left untouched (documenting current, intentional behavior).
	data := `{"a":1} /* not stripped */`
	got := stripLineComments([]byte(data))
	if string(got) != data {
		t.Fatalf("stripLineComments() = %q, want unchanged %q (block comments are not supported)", string(got), data)
	}
}

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
