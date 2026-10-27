package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordingWriteCloser is a simple io.WriteCloser that records writes without
// failing, for exercising "input is present and write succeeds" branches.
type recordingWriteCloser struct {
	writes []string
	closed bool
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func (w *recordingWriteCloser) Close() error {
	w.closed = true
	return nil
}

func TestClaudeAdvanceLoginPromptNoopsWithoutRunningProcess(t *testing.T) {
	t.Parallel()

	var nilGate *claudeAuthGate
	nilGate.advanceLoginPrompt() // nil receiver guard

	g := &claudeAuthGate{}
	g.advanceLoginPrompt() // procRunning false, procInput nil
}

func TestClaudeResubmitPendingBrowserCodeNoopsWithoutPendingState(t *testing.T) {
	t.Parallel()

	var nilGate *claudeAuthGate
	nilGate.resubmitPendingBrowserCode() // nil receiver guard

	g := &claudeAuthGate{}
	g.resubmitPendingBrowserCode() // procRunning false
}

func TestClaudeResubmitPendingBrowserCodeAlternatesCodeOnlyVariant(t *testing.T) {
	t.Parallel()

	input := &recordingWriteCloser{}
	g := &claudeAuthGate{
		procRunning:           true,
		procInput:             input,
		pendingBrowserCode:    "browser-code#test-state",
		browserSubmitAttempts: 1, // odd attempt triggers the code-only variant
		lastBrowserSubmit:     time.Time{},
		logf:                  func(string, ...any) {},
	}

	g.resubmitPendingBrowserCode()

	if len(input.writes) != 1 {
		t.Fatalf("writes = %v, want exactly one resubmission", input.writes)
	}
	if got, want := input.writes[0], "browser-code\n"; got != want {
		t.Fatalf("resubmitted write = %q, want %q (code-only variant)", got, want)
	}
}

func TestClaudeResubmitPendingBrowserCodeHandlesWriteFailure(t *testing.T) {
	t.Parallel()

	g := &claudeAuthGate{
		procRunning:        true,
		procInput:          failingWriteCloser{},
		pendingBrowserCode: "some-code",
		lastBrowserSubmit:  time.Time{},
		logf:               func(string, ...any) {},
	}

	g.resubmitPendingBrowserCode() // should not panic; logs a warning internally
}

func TestClaudeAuthGateStartDeviceAuthFailsWhenCommandCannotStart(t *testing.T) {
	clearClaudeCredentialSignals(t)
	t.Setenv("GH_TOKEN", "github_token_ready")

	// An already-canceled base context makes cmd.Start() fail deterministically
	// regardless of whether the login command is wrapped by a "script" pty
	// helper, without needing to mutate the process-wide PATH (which would be
	// unsafe alongside this package's many concurrently-running tests).
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	g := &claudeAuthGate{
		baseCtx:   canceledCtx,
		command:   filepath.Join(t.TempDir(), "missing-claude-binary"),
		required:  true,
		state:     "needs_browser_login",
		message:   "auth required",
		updatedAt: time.Now().UTC(),
		logf:      func(string, ...any) {},
	}

	status, err := g.StartDeviceAuth(context.Background())
	if err == nil {
		t.Fatal("StartDeviceAuth() error = nil, want command start failure")
	}
	if status.State != "error" {
		t.Fatalf("status = %+v, want error state", status)
	}
	if !strings.Contains(status.Message, "start") {
		t.Fatalf("message = %q, want start-failure detail", status.Message)
	}
}

func TestNormalizeClaudeCredentialsJSONRejectsNullAndTrailingData(t *testing.T) {
	t.Parallel()

	if _, looksLikeJSON, err := normalizeClaudeCredentialsJSON("   "); looksLikeJSON || err != nil {
		t.Fatalf("normalizeClaudeCredentialsJSON(blank) = (_, %v, %v), want (false, nil)", looksLikeJSON, err)
	}
	if _, looksLikeJSON, err := normalizeClaudeCredentialsJSON("not-json"); looksLikeJSON || err != nil {
		t.Fatalf("normalizeClaudeCredentialsJSON(not-json) = (_, %v, %v), want (false, nil)", looksLikeJSON, err)
	}
	if _, looksLikeJSON, _ := normalizeClaudeCredentialsJSON("null"); looksLikeJSON {
		t.Fatalf("normalizeClaudeCredentialsJSON(null) looksLikeJSON = %v, want false (no leading brace)", looksLikeJSON)
	}
	if _, looksLikeJSON, err := normalizeClaudeCredentialsJSON(`{"a":1} trailing`); !looksLikeJSON || err == nil {
		t.Fatalf("normalizeClaudeCredentialsJSON(trailing) = (_, %v, %v), want (true, non-nil)", looksLikeJSON, err)
	}
	if _, looksLikeJSON, err := normalizeClaudeCredentialsJSON(`{"a":1}{"b":2}`); !looksLikeJSON || err == nil {
		t.Fatalf("normalizeClaudeCredentialsJSON(two objects) = (_, %v, %v), want (true, non-nil)", looksLikeJSON, err)
	}
}

func TestExtractClaudeOAuthTokenFromJSONValueHandlesArraysAndFallbackNesting(t *testing.T) {
	t.Parallel()

	array := []any{"not-a-token", "sk-ant-array-token"}
	if got, want := extractClaudeOAuthTokenFromJSONValue(array), "sk-ant-array-token"; got != want {
		t.Fatalf("extractClaudeOAuthTokenFromJSONValue(array) = %q, want %q", got, want)
	}

	// None of the well-known keys match, so the function must fall back to
	// scanning all remaining map values.
	nested := map[string]any{
		"unrelatedField": map[string]any{
			"stillNotIt": "nope",
			"deepValue":  "sk-ant-nested-fallback",
		},
	}
	if got, want := extractClaudeOAuthTokenFromJSONValue(nested), "sk-ant-nested-fallback"; got != want {
		t.Fatalf("extractClaudeOAuthTokenFromJSONValue(nested fallback) = %q, want %q", got, want)
	}

	if got := extractClaudeOAuthTokenFromJSONValue(42); got != "" {
		t.Fatalf("extractClaudeOAuthTokenFromJSONValue(unsupported type) = %q, want empty", got)
	}
	if got := extractClaudeOAuthTokenFromJSONValue("not-a-token"); got != "" {
		t.Fatalf("extractClaudeOAuthTokenFromJSONValue(non-token string) = %q, want empty", got)
	}
}

func TestExtractClaudeAuthStateFromURLHandlesParseErrors(t *testing.T) {
	t.Parallel()

	if got := extractClaudeAuthStateFromURL("%zz"); got != "" {
		t.Fatalf("extractClaudeAuthStateFromURL(invalid) = %q, want empty", got)
	}
}

// TestProbeClaudeAuthStatusClassifiesOutputVariants deliberately does not mark
// its subtests parallel: writing a fresh executable script and immediately
// exec'ing it from many goroutines at once is a known source of spurious
// "text file busy" (ETXTBSY) failures on this sandbox's filesystem, verified
// by direct reproduction outside the testing package. Running the subtests
// sequentially avoids that race while still exercising every probe-output
// classification branch.
func TestProbeClaudeAuthStatusClassifiesOutputVariants(t *testing.T) {
	writeProbeScript := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "claude-probe.sh")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		return path
	}

	t.Run("empty-output", func(t *testing.T) {
		g := &claudeAuthGate{command: writeProbeScript(t, "exit 0\n")}
		ready, msg := g.probeClaudeAuthStatus()
		if ready || msg != "" {
			t.Fatalf("probeClaudeAuthStatus(empty) = (%v, %q), want (false, \"\")", ready, msg)
		}
	})

	t.Run("logged-in-false", func(t *testing.T) {
		g := &claudeAuthGate{command: writeProbeScript(t, `echo '{"loggedIn":false}'`)}
		ready, msg := g.probeClaudeAuthStatus()
		if ready || msg != claudeBrowserLoginRequiredMessage() {
			t.Fatalf("probeClaudeAuthStatus(loggedIn=false) = (%v, %q), want login guidance", ready, msg)
		}
	})

	t.Run("logged-in-no-method-or-provider", func(t *testing.T) {
		g := &claudeAuthGate{command: writeProbeScript(t, `echo '{"loggedIn":true}'`)}
		ready, msg := g.probeClaudeAuthStatus()
		if !ready || msg != "Claude Code and GitHub token are ready." {
			t.Fatalf("probeClaudeAuthStatus(loggedIn=true, no method) = (%v, %q)", ready, msg)
		}
	})

	t.Run("logged-in-with-method-and-provider", func(t *testing.T) {
		g := &claudeAuthGate{command: writeProbeScript(t, `echo '{"loggedIn":true,"authMethod":"session","apiProvider":"firstParty"}'`)}
		ready, msg := g.probeClaudeAuthStatus()
		if !ready || !strings.Contains(msg, "firstParty/session") {
			t.Fatalf("probeClaudeAuthStatus(loggedIn=true, with method) = (%v, %q)", ready, msg)
		}
	})

	t.Run("non-json-loggedin-marker", func(t *testing.T) {
		g := &claudeAuthGate{command: writeProbeScript(t, `echo 'prefix "loggedIn": true suffix'`)}
		ready, msg := g.probeClaudeAuthStatus()
		if !ready || msg != "Claude Code and GitHub token are ready." {
			t.Fatalf("probeClaudeAuthStatus(non-json marker) = (%v, %q)", ready, msg)
		}
	})

	t.Run("unrecognized-output", func(t *testing.T) {
		g := &claudeAuthGate{command: writeProbeScript(t, `echo 'nonsense output'`)}
		ready, msg := g.probeClaudeAuthStatus()
		if ready || msg != "" {
			t.Fatalf("probeClaudeAuthStatus(unrecognized) = (%v, %q), want (false, \"\")", ready, msg)
		}
	})
}

func TestIngestLoginLineNilGateIsNoop(t *testing.T) {
	t.Parallel()

	var nilGate *claudeAuthGate
	nilGate.ingestLoginLine("https://claude.ai/login")
}

func TestIngestLoginLineAdvancesPromptWhenGatingWindowElapsed(t *testing.T) {
	t.Parallel()

	input := &recordingWriteCloser{}
	g := &claudeAuthGate{
		procRunning:       true,
		procInput:         input,
		lastPromptAdvance: time.Time{},
		logf:              func(string, ...any) {},
	}

	g.ingestLoginLine("Please select an option below")

	if len(input.writes) == 0 {
		t.Fatal("expected prompt-advance write to the login process stdin")
	}
}

func TestIngestLoginLineCapturesOAuthTokenCandidate(t *testing.T) {
	clearClaudeCredentialSignals(t)

	g := &claudeAuthGate{logf: func(string, ...any) {}}
	g.ingestLoginLine("Paste your OAuth token here:")
	g.mu.Lock()
	awaiting := g.awaitingOAuthToken
	g.mu.Unlock()
	if !awaiting {
		t.Fatal("expected awaitingOAuthToken to be set after token prompt line")
	}

	g.ingestLoginLine(strings.Repeat("a", 30))

	status, err := g.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("status = %+v, want ready after capturing oauth token candidate", status)
	}
}

func TestIngestLoginLineStopsAwaitingTokenOnTerminalPhrase(t *testing.T) {
	t.Parallel()

	g := &claudeAuthGate{awaitingOAuthToken: true, logf: func(string, ...any) {}}
	g.ingestLoginLine("Login successful, you're all set")

	g.mu.Lock()
	awaiting := g.awaitingOAuthToken
	g.mu.Unlock()
	if awaiting {
		t.Fatal("expected awaitingOAuthToken to clear after terminal phrase")
	}
}

func TestIngestLoginLineResubmitsCodeOnPromptLine(t *testing.T) {
	t.Parallel()

	input := &recordingWriteCloser{}
	g := &claudeAuthGate{
		procRunning:        true,
		procInput:          input,
		pendingBrowserCode: "some-code",
		lastBrowserSubmit:  time.Time{},
		logf:               func(string, ...any) {},
	}

	g.ingestLoginLine("Paste code here >")

	if len(input.writes) == 0 {
		t.Fatal("expected pending browser code to be resubmitted")
	}
}

func TestShouldLogClaudeLoginLineEmptyInput(t *testing.T) {
	t.Parallel()

	if shouldLogClaudeLoginLine("   ") {
		t.Fatal("shouldLogClaudeLoginLine(blank) = true, want false")
	}
}

func TestTruncateClaudeAuthLineTruncatesLongLines(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 400)
	got := truncateClaudeAuthLine(long)
	if len(got) != 240 {
		t.Fatalf("len(truncateClaudeAuthLine(long)) = %d, want 240", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncateClaudeAuthLine(long) = %q, want ... suffix", got)
	}
}

func TestExtractClaudeAuthURLSkipsInvalidAndHostlessCandidates(t *testing.T) {
	t.Parallel()

	if got := extractClaudeAuthURL("see https://example.com/%zz for details"); got != "" {
		t.Fatalf("extractClaudeAuthURL(invalid escape) = %q, want empty", got)
	}
	if got := extractClaudeAuthURL("open https://#fragment-only now"); got != "" {
		t.Fatalf("extractClaudeAuthURL(hostless) = %q, want empty", got)
	}
	if got, want := extractClaudeAuthURL("visit https://claude.ai/login/device now"), "https://claude.ai/login/device"; got != want {
		t.Fatalf("extractClaudeAuthURL(valid) = %q, want %q", got, want)
	}
}

func TestSanitizeClaudeAuthURLHandlesBlankInput(t *testing.T) {
	t.Parallel()

	if got := sanitizeClaudeAuthURL("   "); got != "" {
		t.Fatalf("sanitizeClaudeAuthURL(blank) = %q, want empty", got)
	}
}

func TestShouldAdvanceClaudeLoginPromptEmptyInput(t *testing.T) {
	t.Parallel()

	if shouldAdvanceClaudeLoginPrompt("   ") {
		t.Fatal("shouldAdvanceClaudeLoginPrompt(blank) = true, want false")
	}
}

func TestShouldPromptForClaudeBrowserCodeEmptyInput(t *testing.T) {
	t.Parallel()

	if shouldPromptForClaudeBrowserCode("   ") {
		t.Fatal("shouldPromptForClaudeBrowserCode(blank) = true, want false")
	}
}

func TestShouldCaptureClaudeOAuthTokenEmptyInput(t *testing.T) {
	t.Parallel()

	if shouldCaptureClaudeOAuthToken("   ") {
		t.Fatal("shouldCaptureClaudeOAuthToken(blank) = true, want false")
	}
}

func TestExtractClaudeOAuthTokenCandidateRejectsNonCandidates(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"":                               "",
		"   ":                            "",
		"two words here":                 "",
		"https://example.com/oauth/code": "",
		"short":                          "",
	}
	for input, want := range tests {
		if got := extractClaudeOAuthTokenCandidate(input); got != want {
			t.Errorf("extractClaudeOAuthTokenCandidate(%q) = %q, want %q", input, got, want)
		}
	}
	if got, want := extractClaudeOAuthTokenCandidate(strings.Repeat("b", 30)), strings.Repeat("b", 30); got != want {
		t.Fatalf("extractClaudeOAuthTokenCandidate(valid) = %q, want %q", got, want)
	}
}

func TestCompleteWithClaudeOAuthTokenNilGateAndEmptyToken(t *testing.T) {
	t.Parallel()

	var nilGate *claudeAuthGate
	nilGate.completeWithClaudeOAuthToken("sk-ant-ignored") // nil receiver guard

	g := &claudeAuthGate{logf: func(string, ...any) {}}
	g.completeWithClaudeOAuthToken("   ") // empty token guard; state unchanged
	if g.ready {
		t.Fatal("completeWithClaudeOAuthToken(blank) unexpectedly marked gate ready")
	}
}

func TestCompleteWithClaudeOAuthTokenClosesInputAndCancelsProcess(t *testing.T) {
	clearClaudeCredentialSignals(t)

	input := &recordingWriteCloser{}
	canceled := false
	g := &claudeAuthGate{
		procRunning: true,
		procInput:   input,
		procCancel:  func() { canceled = true },
		logf:        func(string, ...any) {},
	}

	g.completeWithClaudeOAuthToken("sk-ant-direct-token")

	if !input.closed {
		t.Fatal("expected process stdin to be closed on successful oauth token capture")
	}
	if !canceled {
		t.Fatal("expected process to be canceled on successful oauth token capture")
	}
}

func TestCompleteWithClaudeOAuthTokenLogsWarnWhenPersistFails(t *testing.T) {
	clearClaudeCredentialSignals(t)

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var logged []string
	g := &claudeAuthGate{
		runtimeConfigPath: path,
		logf: func(format string, args ...any) {
			logged = append(logged, format)
		},
	}

	g.completeWithClaudeOAuthToken("sk-ant-persist-failure")

	found := false
	for _, line := range logged {
		if strings.Contains(line, "persist_oauth_token") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logged = %v, want a persist_oauth_token warning", logged)
	}
}

func TestCompleteWithClaudeCredentialsJSONNilGate(t *testing.T) {
	t.Parallel()

	var nilGate *claudeAuthGate
	if err := nilGate.completeWithClaudeCredentialsJSON(`{"a":1}`); err != nil {
		t.Fatalf("completeWithClaudeCredentialsJSON(nil) error = %v, want nil", err)
	}
}

func TestCompleteWithClaudeCredentialsJSONPropagatesSaveFailure(t *testing.T) {
	clearClaudeCredentialSignals(t)

	home := os.Getenv("HOME")
	blockerPath := filepath.Join(home, ".claude")
	if err := os.WriteFile(blockerPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocker) error = %v", err)
	}

	g := &claudeAuthGate{logf: func(string, ...any) {}}
	err := g.completeWithClaudeCredentialsJSON(`{"claudeAiOauth":{"accessToken":"sk-ant-x"}}`)
	if err == nil {
		t.Fatal("completeWithClaudeCredentialsJSON() error = nil, want save failure")
	}
}

func TestCompleteWithClaudeCredentialsJSONPropagatesClearOverrideFailure(t *testing.T) {
	clearClaudeCredentialSignals(t)

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	g := &claudeAuthGate{runtimeConfigPath: configPath, logf: func(string, ...any) {}}
	err := g.completeWithClaudeCredentialsJSON(`{"claudeAiOauth":{"accessToken":"sk-ant-x"}}`)
	if err == nil || !strings.Contains(err.Error(), "activate Claude credentials file") {
		t.Fatalf("completeWithClaudeCredentialsJSON() error = %v, want wrapped clear-override failure", err)
	}
}

func TestCompleteWithClaudeCredentialsJSONClosesInputAndCancelsProcess(t *testing.T) {
	clearClaudeCredentialSignals(t)

	input := &recordingWriteCloser{}
	canceled := false
	g := &claudeAuthGate{
		procRunning: true,
		procInput:   input,
		procCancel:  func() { canceled = true },
		logf:        func(string, ...any) {},
	}

	if err := g.completeWithClaudeCredentialsJSON(`{"claudeAiOauth":{"accessToken":"sk-ant-x"}}`); err != nil {
		t.Fatalf("completeWithClaudeCredentialsJSON() error = %v", err)
	}
	if !input.closed {
		t.Fatal("expected process stdin to be closed")
	}
	if !canceled {
		t.Fatal("expected process to be canceled")
	}
}

func TestBuildClaudeLoginCommandDefaultsEmptyCommand(t *testing.T) {
	t.Parallel()

	cmd := buildClaudeLoginCommand(context.Background(), "   ")
	if cmd == nil {
		t.Fatal("buildClaudeLoginCommand() = nil")
	}
}

func TestShellQuoteEmptyValue(t *testing.T) {
	t.Parallel()

	if got, want := shellQuote("   "), "''"; got != want {
		t.Fatalf("shellQuote(blank) = %q, want %q", got, want)
	}
}

func TestFirstConfiguredClaudeOAuthTokenFallsBackToRuntimeConfig(t *testing.T) {
	t.Setenv(claudeOAuthTokenEnv, "")

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"claude_code_oauth_token":"sk-ant-persisted"}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, source := firstConfiguredClaudeOAuthToken(path)
	if got != "sk-ant-persisted" || source != "runtime config" {
		t.Fatalf("firstConfiguredClaudeOAuthToken() = (%q, %q), want (sk-ant-persisted, runtime config)", got, source)
	}
}

func TestSetClaudeOAuthTokenEnvironmentRejectsEmptyToken(t *testing.T) {
	t.Parallel()

	if err := setClaudeOAuthTokenEnvironment("   "); err == nil {
		t.Fatal("setClaudeOAuthTokenEnvironment(blank) error = nil, want required error")
	}
}

func TestClearClaudeOAuthTokenOverridePropagatesParseFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := clearClaudeOAuthTokenOverride(path); err == nil {
		t.Fatal("clearClaudeOAuthTokenOverride(malformed config) error = nil, want parse failure")
	}
}

func TestPendingBrowserLoginFailureDefaultsEmptyState(t *testing.T) {
	t.Parallel()

	g := &claudeAuthGate{}
	snap, err := g.pendingBrowserLoginFailure("boom detail", errors.New("boom"))
	if err == nil {
		t.Fatal("pendingBrowserLoginFailure() error = nil, want boom")
	}
	if snap.State != "needs_configure" {
		t.Fatalf("snap.State = %q, want needs_configure default", snap.State)
	}
	if snap.Message != "boom detail" {
		t.Fatalf("snap.Message = %q, want boom detail", snap.Message)
	}
}

func TestIsClaudeDocsURLRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	if isClaudeDocsURL("%zz") {
		t.Fatal("isClaudeDocsURL(invalid) = true, want false")
	}
}
