package main

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestNewDefaultTerminalLoggerFallsBackWhenLogMirrorFails(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	t.Setenv("HARNESS_WORKSPACE_RAM_BASE", tmp)
	t.Setenv("HARNESS_WORKSPACE_DISK_BASE", tmp)
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%q) error = %v", tmp, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	// Removing write permission on tmp prevents both the primary (RAM base)
	// and fallback (cwd) log root directories from being created, forcing
	// newDefaultTaskLogMirror (and therefore newDefaultTerminalLogger) down
	// its error path.
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatalf("Chmod(tmp) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })

	logger := newDefaultTerminalLogger()
	if logger == nil {
		t.Fatal("newDefaultTerminalLogger() = nil, want fallback logger")
	}
	if logger.sink != nil {
		t.Fatal("newDefaultTerminalLogger() sink set despite mirror init failure, want nil")
	}
}

func TestIsTerminalFileReturnsFalseOnStatError(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if isTerminalFile(f) {
		t.Fatal("isTerminalFile(closed file) = true, want false")
	}
}

func TestTerminalLoggerPrintDropsEmptyRenderedLine(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	sink := &recordingTerminalLogSink{}
	logger := newTerminalLogger(&out, false)
	logger.sink = sink

	logger.Print("   ")

	if out.String() != "" {
		t.Fatalf("terminal output = %q, want empty", out.String())
	}
	if len(sink.lines) != 0 {
		t.Fatalf("sink lines = %v, want none", sink.lines)
	}
}

func TestTerminalLoggerPrintProgressLineInNonTTYModeWritesPlainLine(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	sink := &recordingTerminalLogSink{}
	logger := newTerminalLogger(&out, false)
	logger.sink = sink

	logger.Print("stage=claude status=running elapsed_s=12")

	if got := out.String(); got != "claude running... 12s\n" {
		t.Fatalf("terminal output = %q, want compact progress line with newline", got)
	}
	if len(sink.lines) != 1 || sink.lines[0] != "claude running... 12s" {
		t.Fatalf("sink lines = %v, want compact progress line", sink.lines)
	}
	if logger.progressActive {
		t.Fatal("progressActive = true in non-tty mode, want false")
	}
}

func TestTerminalLoggerCloseEmitsNewlineWhenProgressActive(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	logger := newTerminalLogger(&out, true)
	logger.Print("stage=claude status=running elapsed_s=1")

	if !logger.progressActive {
		t.Fatal("progressActive = false after tty progress render, want true")
	}

	logger.Close()

	if logger.progressActive {
		t.Fatal("progressActive = true after Close(), want false")
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("output = %q, want trailing newline from Close()", out.String())
	}
}

func TestCompactAgentProgressLineRejectsNonCompactableStage(t *testing.T) {
	t.Parallel()

	got, mode := formatTerminalLogLine("stage=git status=running elapsed_s=5")
	if mode != terminalLogModeNormal {
		t.Fatalf("mode = %v, want normal for non-compactable stage", mode)
	}
	if got != "stage=git status=running elapsed_s=5" {
		t.Fatalf("got = %q, want original line unchanged", got)
	}
}

func TestCompactAgentProgressLineWithoutElapsedOmitsSuffix(t *testing.T) {
	t.Parallel()

	got, mode := formatTerminalLogLine("stage=codex status=running")
	if mode != terminalLogModeProgress {
		t.Fatalf("mode = %v, want progress", mode)
	}
	if got != "codex running..." {
		t.Fatalf("got = %q, want %q", got, "codex running...")
	}
}

func TestIsCompactableAgentStageTable(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"agent":  true,
		"codex":  true,
		"claude": true,
		"git":    false,
		"":       false,
	}
	for stage, want := range tests {
		if got := isCompactableAgentStage(stage); got != want {
			t.Errorf("isCompactableAgentStage(%q) = %v, want %v", stage, got, want)
		}
	}
}

func TestDecodeCommandLogLineTruncatesLongPayloads(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", maxDecodedCommandLogText+50)
	line := "cmd phase=clone name=git stream=stdout b64=" + base64.StdEncoding.EncodeToString([]byte(long))

	got, handled, drop := decodeCommandLogLine(line)
	if !handled || drop {
		t.Fatalf("decodeCommandLogLine(long payload) = (%q, %v, %v), want handled and not dropped", got, handled, drop)
	}
	if !strings.Contains(got, "...") {
		t.Fatalf("got = %q, want truncation marker", got)
	}
	if strings.Count(got, "a") >= len(long) {
		t.Fatalf("got = %q, want truncated text shorter than input", got)
	}
}

func TestSimpleKVDigitsOnlyTable(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"":      false,
		"123":   true,
		"12a":   false,
		"a12":   false,
		"00099": true,
	}
	for text, want := range tests {
		if got := simpleKVDigitsOnly(text); got != want {
			t.Errorf("simpleKVDigitsOnly(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestParseSimpleKVFieldsReturnsNilWhenNoEquals(t *testing.T) {
	t.Parallel()

	if got := parseSimpleKVFields("just plain text"); got != nil {
		t.Fatalf("parseSimpleKVFields(no equals) = %v, want nil", got)
	}
}

func TestParseSimpleKVFieldsReturnsNilWhenNoValidTokens(t *testing.T) {
	t.Parallel()

	if got := parseSimpleKVFields("==="); got != nil {
		t.Fatalf("parseSimpleKVFields(malformed) = %v, want nil", got)
	}
}

func TestParseSimpleKVTokensHandlesWhitespaceOnlyAndMalformedInput(t *testing.T) {
	t.Parallel()

	if got := parseSimpleKVTokens("   "); got != nil {
		t.Fatalf("parseSimpleKVTokens(whitespace) = %v, want nil", got)
	}
	if got := parseSimpleKVTokens(""); got != nil {
		t.Fatalf("parseSimpleKVTokens(empty) = %v, want nil", got)
	}

	// A bare token with no '=' is skipped entirely.
	tokens := parseSimpleKVTokens("noequals key=value")
	if len(tokens) != 1 || tokens[0].key != "key" || tokens[0].value != "value" {
		t.Fatalf("tokens = %+v, want single key=value token", tokens)
	}

	// A token with an empty key (leading '=') is skipped.
	tokens = parseSimpleKVTokens("=orphan key=value")
	if len(tokens) != 1 || tokens[0].key != "key" {
		t.Fatalf("tokens = %+v, want orphan equals skipped", tokens)
	}
}

func TestParseSimpleKVValueFallsBackWhenUnquoteFails(t *testing.T) {
	t.Parallel()

	// An unterminated escape sequence inside the quotes makes strconv.Unquote
	// fail; the parser should fall back to trimming the surrounding quotes.
	line := `key="bad \q escape"`
	fields := parseSimpleKVFields(line)
	if got, want := fields["key"], `bad \q escape`; got != want {
		t.Fatalf("fields[key] = %q, want %q", got, want)
	}
}

func TestScanSimpleKVQuotedValueRejectsNonQuotedOrUnterminatedInput(t *testing.T) {
	t.Parallel()

	if _, _, ok := scanSimpleKVQuotedValue("value", 0); ok {
		t.Fatal("scanSimpleKVQuotedValue(non-quoted) ok = true, want false")
	}
	if _, _, ok := scanSimpleKVQuotedValue(`"unterminated`, 0); ok {
		t.Fatal("scanSimpleKVQuotedValue(unterminated) ok = true, want false")
	}
	if _, _, ok := scanSimpleKVQuotedValue(`"ok"`, 10); ok {
		t.Fatal("scanSimpleKVQuotedValue(start past end) ok = true, want false")
	}
}

func TestStripSimpleKVFieldWithEmptyKeyTrimsLine(t *testing.T) {
	t.Parallel()

	if got, want := stripSimpleKVField("  spaced out  ", ""), "spaced out"; got != want {
		t.Fatalf("stripSimpleKVField(empty key) = %q, want %q", got, want)
	}
}
