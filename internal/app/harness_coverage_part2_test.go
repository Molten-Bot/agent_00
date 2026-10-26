package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// This file adds focused unit tests for review-cycle, PR-comment,
// check-reconciliation, and log/env-helper functions defined in the second
// half of internal/app/harness.go (func start line >= ~4200). Helpers in this
// file are prefixed with h2cov to avoid name collisions with tests other
// agents are adding concurrently in the same package.

func h2covReviewFinding(severity, path string, line int, title string) reviewFinding {
	return reviewFinding{Severity: severity, Path: path, Line: line, Title: title}
}

func TestNormalizeReviewCommentPointsLimits(t *testing.T) {
	t.Parallel()

	if got := normalizeReviewCommentPoints([]string{"a", "b"}, 0); got != nil {
		t.Fatalf("normalizeReviewCommentPoints(limit=0) = %v, want nil", got)
	}
	if got := normalizeReviewCommentPoints([]string{"a", "b"}, -1); got != nil {
		t.Fatalf("normalizeReviewCommentPoints(limit<0) = %v, want nil", got)
	}
	got := normalizeReviewCommentPoints([]string{"one", "", "  ", "two", "three"}, 2)
	if want := []string{"one", "two"}; !slicesEqualH2(got, want) {
		t.Fatalf("normalizeReviewCommentPoints(limit=2) = %v, want %v", got, want)
	}
}

func TestReviewFindingPointsLimits(t *testing.T) {
	t.Parallel()

	findings := []reviewFinding{
		h2covReviewFinding("high", "main.go", 10, "bug one"),
		h2covReviewFinding("", "", 0, "bug two"),
		h2covReviewFinding("low", "lib.go", 0, "bug three"),
	}
	if got := reviewFindingPoints(findings, 0); got != nil {
		t.Fatalf("reviewFindingPoints(limit=0) = %v, want nil", got)
	}
	got := reviewFindingPoints(findings, 1)
	if len(got) != 1 {
		t.Fatalf("reviewFindingPoints(limit=1) = %v, want 1 entry", got)
	}
	if !strings.Contains(got[0], "main.go:10") {
		t.Fatalf("reviewFindingPoints(limit=1)[0] = %q, want location with line", got[0])
	}
}

func TestReviewFindingPointBranches(t *testing.T) {
	t.Parallel()

	if got := reviewFindingPoint(h2covReviewFinding("high", "p.go", 5, "")); got != "" {
		t.Fatalf("reviewFindingPoint(blank title) = %q, want empty", got)
	}
	if got := reviewFindingPoint(h2covReviewFinding("", "", 0, "no severity or location")); got != "[Finding] no severity or location" {
		t.Fatalf("reviewFindingPoint(default severity, no location) = %q", got)
	}
	if got := reviewFindingPoint(h2covReviewFinding("high", "", 5, "no path but line")); got != "[high] no path but line" {
		t.Fatalf("reviewFindingPoint(no path, positive line) = %q, want no location suffix", got)
	}
	if got := reviewFindingPoint(h2covReviewFinding("high", "path.go", 0, "path without line")); got != "[high] path.go - path without line" {
		t.Fatalf("reviewFindingPoint(path, zero line) = %q", got)
	}
	if got := reviewFindingPoint(h2covReviewFinding("high", "path.go", 12, "path with line")); got != "[high] path.go:12 - path with line" {
		t.Fatalf("reviewFindingPoint(path, line) = %q", got)
	}
}

func TestCleanReviewCommentPointTruncates(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 300)
	got := cleanReviewCommentPoint("- " + long)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("cleanReviewCommentPoint(long) missing truncation marker: %q", got)
	}
	if len(got) != 220 {
		t.Fatalf("cleanReviewCommentPoint(long) length = %d, want 220", len(got))
	}
}

func TestTruncateReviewCommentBodyTruncates(t *testing.T) {
	t.Parallel()

	short := "short body"
	if got := truncateReviewCommentBody(short); got != short {
		t.Fatalf("truncateReviewCommentBody(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("y", maxReviewCommentBodyChars+100)
	got := truncateReviewCommentBody(long)
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Fatalf("truncateReviewCommentBody(long) missing truncation marker")
	}
	if len(got) >= len(long) {
		t.Fatalf("truncateReviewCommentBody(long) length = %d, want shorter than input", len(got))
	}
}

func TestReviewSummaryBodyPathBranches(t *testing.T) {
	t.Parallel()

	runDir := "/tmp/run"
	if got, want := reviewSummaryBodyPath(runDir, 42), filepath.Join(runDir, "review-summary-pr-42.md"); got != want {
		t.Fatalf("reviewSummaryBodyPath(positive pr) = %q, want %q", got, want)
	}
	if got, want := reviewSummaryBodyPath(runDir, 0), filepath.Join(runDir, "review-summary.md"); got != want {
		t.Fatalf("reviewSummaryBodyPath(no pr) = %q, want %q", got, want)
	}
	if got, want := reviewSummaryBodyPath(runDir, -1), filepath.Join(runDir, "review-summary.md"); got != want {
		t.Fatalf("reviewSummaryBodyPath(negative pr) = %q, want %q", got, want)
	}
}

func TestPrMergeAutoCommandMergeMethods(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		method string
		want   string
	}{
		{method: "merge", want: "--merge"},
		{method: "rebase", want: "--rebase"},
		{method: "squash", want: "--squash"},
		{method: "unknown", want: "--squash"},
		{method: "", want: "--squash"},
	} {
		cmd := prMergeAutoCommand("/repo", "42", "acme/repo", tc.method, "abc123")
		if !slicesContains(cmd.Args, tc.want) {
			t.Fatalf("prMergeAutoCommand(method=%q).Args = %v, want %q", tc.method, cmd.Args, tc.want)
		}
	}

	withRepo := prMergeAutoCommand("/repo", "42", "acme/repo", "merge", "abc123")
	if !slicesContains(withRepo.Args, "--repo") {
		t.Fatalf("prMergeAutoCommand(targetRepo) args = %v, want --repo", withRepo.Args)
	}
	withoutRepo := prMergeAutoCommand("/repo", "42", "", "merge", "abc123")
	if slicesContains(withoutRepo.Args, "--repo") {
		t.Fatalf("prMergeAutoCommand(no targetRepo) args = %v, want no --repo", withoutRepo.Args)
	}
}

func TestBaseSyncConflictPromptEmptyMergeSummary(t *testing.T) {
	t.Parallel()

	got := baseSyncConflictPrompt("base task", "/repo", "git@github.com:acme/repo.git", "work", "main", execx.Result{})
	if !strings.Contains(got, "git merge reported conflicts but did not provide output.") {
		t.Fatalf("baseSyncConflictPrompt(empty merge result) missing fallback summary: %q", got)
	}

	withOutput := baseSyncConflictPrompt("base task", "/repo", "git@github.com:acme/repo.git", "work", "main", execx.Result{Stderr: "CONFLICT (content): x"})
	if !strings.Contains(withOutput, "CONFLICT (content): x") {
		t.Fatalf("baseSyncConflictPrompt(with merge output) missing merge summary: %q", withOutput)
	}
}

func TestRemoteBranchSyncConflictPromptDefaultsRemote(t *testing.T) {
	t.Parallel()

	got := remoteBranchSyncConflictPrompt("base task", "/repo", "git@github.com:acme/repo.git", "work", "", execx.Result{})
	if !strings.Contains(got, "Remote being merged: origin/work") {
		t.Fatalf("remoteBranchSyncConflictPrompt(blank remote) missing default origin: %q", got)
	}
	if !strings.Contains(got, "git merge reported conflicts but did not provide output.") {
		t.Fatalf("remoteBranchSyncConflictPrompt(empty merge result) missing fallback summary: %q", got)
	}

	withRemote := remoteBranchSyncConflictPrompt("base task", "/repo", "git@github.com:acme/repo.git", "work", "upstream", execx.Result{Stderr: "CONFLICT"})
	if !strings.Contains(withRemote, "Remote being merged: upstream/work") {
		t.Fatalf("remoteBranchSyncConflictPrompt(explicit remote) = %q, want upstream/work", withRemote)
	}
}

func TestIsGitConfigLockContentionError(t *testing.T) {
	t.Parallel()

	if isGitConfigLockContentionError(nil) {
		t.Fatal("isGitConfigLockContentionError(nil) = true, want false")
	}
	for _, msg := range []string{
		"error: could not lock config file /repo/.git/config: File exists",
		"unable to write .gitconfig: file exists",
		"fatal: .gitconfig.lock already exists",
		"another git process seems to be running in this repository",
	} {
		if !isGitConfigLockContentionError(errors.New(msg)) {
			t.Fatalf("isGitConfigLockContentionError(%q) = false, want true", msg)
		}
	}
	if isGitConfigLockContentionError(errors.New("fatal: not a git repository")) {
		t.Fatal("isGitConfigLockContentionError(unrelated) = true, want false")
	}
}

func TestIsCodexExecutionTransportFailure(t *testing.T) {
	t.Parallel()

	if isCodexExecutionTransportFailure(execx.Result{}, nil) {
		t.Fatal("isCodexExecutionTransportFailure(nil err) = true, want false")
	}
	for _, msg := range []string{
		"error from backend-api/codex/responses",
		"responses_websocket disconnected",
		"stream disconnected before completion",
		"failed to connect to websocket",
		"failed to lookup address information for host",
		"temporary failure in name resolution",
	} {
		if !isCodexExecutionTransportFailure(execx.Result{}, errors.New(msg)) {
			t.Fatalf("isCodexExecutionTransportFailure(%q) = false, want true", msg)
		}
	}
	if isCodexExecutionTransportFailure(execx.Result{Stdout: "ok", Stderr: "ok"}, errors.New("some other failure")) {
		t.Fatal("isCodexExecutionTransportFailure(unrelated) = true, want false")
	}
	// Markers found only in captured output (not the error string) must still match.
	if !isCodexExecutionTransportFailure(execx.Result{Stderr: "stream disconnected before completion"}, errors.New("exit status 1")) {
		t.Fatal("isCodexExecutionTransportFailure(marker in stderr) = false, want true")
	}
}

func TestWriteWorkspaceFileFallsBackToOSWriteFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "file.txt")
	h := Harness{}
	if err := h.writeWorkspaceFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("writeWorkspaceFile() error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("writeWorkspaceFile() wrote %q, want hello", string(got))
	}
}

func TestWriteWorkspaceFileUsesWorkspaceOverride(t *testing.T) {
	t.Parallel()

	var capturedPath string
	var capturedData []byte
	h := Harness{}
	h.Workspace.WriteFile = func(path string, data []byte, _ os.FileMode) error {
		capturedPath = path
		capturedData = data
		return nil
	}
	if err := h.writeWorkspaceFile("/virtual/path.txt", []byte("payload"), 0o600); err != nil {
		t.Fatalf("writeWorkspaceFile() error = %v", err)
	}
	if capturedPath != "/virtual/path.txt" || string(capturedData) != "payload" {
		t.Fatalf("writeWorkspaceFile() did not invoke override correctly: path=%q data=%q", capturedPath, capturedData)
	}
}

func TestLogKVAndIsSimpleLogKVValue(t *testing.T) {
	t.Parallel()

	if got := logKV("", "value"); got != "" {
		t.Fatalf("logKV(empty key) = %q, want empty", got)
	}
	if got := logKV("key", ""); got != "" {
		t.Fatalf("logKV(empty value) = %q, want empty", got)
	}
	if got := logKV("stage", "clone-retry/1"); got != "stage=clone-retry/1" {
		t.Fatalf("logKV(simple value) = %q", got)
	}
	if got := logKV("message", "has a space"); got != `message="has a space"` {
		t.Fatalf("logKV(complex value) = %q, want quoted", got)
	}
	if isSimpleLogKVValue("") {
		t.Fatal("isSimpleLogKVValue(empty) = true, want false")
	}
	if !isSimpleLogKVValue("Agent00-run_1.2/3:4@5") {
		t.Fatal("isSimpleLogKVValue(allowed charset) = false, want true")
	}
	if isSimpleLogKVValue("has space") {
		t.Fatal("isSimpleLogKVValue(space) = true, want false")
	}
	if isSimpleLogKVValue(`has"quote`) {
		t.Fatal(`isSimpleLogKVValue(quote) = true, want false`)
	}
}

func TestStageAgentsPromptFileErrorsAndStaging(t *testing.T) {
	t.Parallel()

	if _, _, err := stageAgentsPromptFile("", "/tmp/AGENTS.md"); err == nil {
		t.Fatal("stageAgentsPromptFile(empty targetDir) error = nil, want non-nil")
	}
	if _, _, err := stageAgentsPromptFile("/tmp/target", ""); err == nil {
		t.Fatal("stageAgentsPromptFile(empty agentsPath) error = nil, want non-nil")
	}

	targetDir := t.TempDir()
	withinPath := filepath.Join(targetDir, "AGENTS.md")
	if err := os.WriteFile(withinPath, []byte("inside"), 0o644); err != nil {
		t.Fatalf("WriteFile(within) error = %v", err)
	}
	stagedPath, cleanup, err := stageAgentsPromptFile(targetDir, withinPath)
	if err != nil {
		t.Fatalf("stageAgentsPromptFile(within target) error = %v", err)
	}
	if stagedPath != withinPath {
		t.Fatalf("stageAgentsPromptFile(within target) = %q, want unchanged path %q", stagedPath, withinPath)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}

	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "AGENTS.md")
	if err := os.WriteFile(outsidePath, []byte("outside content"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside) error = %v", err)
	}
	stagedPath, cleanup, err = stageAgentsPromptFile(targetDir, outsidePath)
	if err != nil {
		t.Fatalf("stageAgentsPromptFile(outside target) error = %v", err)
	}
	if stagedPath == outsidePath {
		t.Fatal("stageAgentsPromptFile(outside target) did not stage a copy")
	}
	data, err := os.ReadFile(stagedPath)
	if err != nil || string(data) != "outside content" {
		t.Fatalf("staged file content = (%q, %v), want outside content", string(data), err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged file still exists after cleanup: %v", err)
	}
	// Cleanup must be idempotent (a missing file is not an error).
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() second call error = %v", err)
	}

	// Force the os.CreateTemp failure branch with a nonexistent target directory.
	if _, _, err := stageAgentsPromptFile(filepath.Join(outsideDir, "missing-dir"), outsidePath); err == nil {
		t.Fatal("stageAgentsPromptFile(missing target dir) error = nil, want non-nil")
	}
}

func TestSelectAgentsPromptFileBranches(t *testing.T) {
	t.Parallel()

	if _, _, err := selectAgentsPromptFile("", "/tmp/AGENTS.md"); err == nil {
		t.Fatal("selectAgentsPromptFile(empty targetDir) error = nil, want non-nil")
	}
	if _, _, err := selectAgentsPromptFile("/tmp/target", ""); err == nil {
		t.Fatal("selectAgentsPromptFile(empty agentsPath) error = nil, want non-nil")
	}

	targetDir := t.TempDir()
	fallbackPath := filepath.Join(t.TempDir(), "AGENTS.md")
	path, cleanup, err := selectAgentsPromptFile(targetDir, fallbackPath)
	if err != nil {
		t.Fatalf("selectAgentsPromptFile(no target AGENTS.md) error = %v", err)
	}
	if path != fallbackPath {
		t.Fatalf("selectAgentsPromptFile(no target AGENTS.md) = %q, want fallback %q", path, fallbackPath)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}

	existingPath := filepath.Join(targetDir, "AGENTS.md")
	if err := os.WriteFile(existingPath, []byte("existing"), 0o644); err != nil {
		t.Fatalf("WriteFile(existing) error = %v", err)
	}
	path, _, err = selectAgentsPromptFile(targetDir, fallbackPath)
	if err != nil {
		t.Fatalf("selectAgentsPromptFile(existing target AGENTS.md) error = %v", err)
	}
	if path != existingPath {
		t.Fatalf("selectAgentsPromptFile(existing target AGENTS.md) = %q, want %q", path, existingPath)
	}

	if err := os.Remove(existingPath); err != nil {
		t.Fatalf("Remove(existingPath) error = %v", err)
	}
	if err := os.MkdirAll(existingPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(AGENTS.md as dir) error = %v", err)
	}
	if _, _, err := selectAgentsPromptFile(targetDir, fallbackPath); err == nil {
		t.Fatal("selectAgentsPromptFile(target AGENTS.md is a dir) error = nil, want non-nil")
	}
}

func TestSeedAgentConfigDirBranches(t *testing.T) {
	t.Parallel()

	if err := seedAgentConfigDir("", "/tmp/target"); err != nil {
		t.Fatalf("seedAgentConfigDir(empty source) error = %v, want nil", err)
	}
	if err := seedAgentConfigDir("/tmp/source", ""); err != nil {
		t.Fatalf("seedAgentConfigDir(empty target) error = %v, want nil", err)
	}

	sameDir := t.TempDir()
	if err := seedAgentConfigDir(sameDir, sameDir); err != nil {
		t.Fatalf("seedAgentConfigDir(same path) error = %v, want nil", err)
	}

	missingSource := filepath.Join(t.TempDir(), "does-not-exist")
	targetDir := t.TempDir()
	if err := seedAgentConfigDir(missingSource, targetDir); err != nil {
		t.Fatalf("seedAgentConfigDir(missing source) error = %v, want nil", err)
	}

	// Symlinked source must be skipped entirely (never followed or copied).
	linkTargetDir := t.TempDir()
	symlinkSource := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(linkTargetDir, symlinkSource); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}
	if err := seedAgentConfigDir(symlinkSource, targetDir); err != nil {
		t.Fatalf("seedAgentConfigDir(symlink source) error = %v, want nil", err)
	}

	// Regular file source is copied directly into targetDir.
	fileSourceDir := t.TempDir()
	fileSource := filepath.Join(fileSourceDir, "auth.json")
	if err := os.WriteFile(fileSource, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile(fileSource) error = %v", err)
	}
	fileTargetDir := t.TempDir()
	if err := seedAgentConfigDir(fileSource, fileTargetDir); err != nil {
		t.Fatalf("seedAgentConfigDir(file source) error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fileTargetDir, "auth.json")); err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("seedAgentConfigDir(file source) copy = (%q, %v)", string(got), err)
	}

	// Directory source: regular files copied, symlinks and subdirectories skipped.
	dirSource := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirSource, "a.json"), []byte("a"), 0o600); err != nil {
		t.Fatalf("WriteFile(a.json) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dirSource, "subdir"), 0o755); err != nil {
		t.Fatalf("MkdirAll(subdir) error = %v", err)
	}
	if err := os.Symlink(filepath.Join(dirSource, "a.json"), filepath.Join(dirSource, "link.json")); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}
	dirTarget := t.TempDir()
	if err := seedAgentConfigDir(dirSource, dirTarget); err != nil {
		t.Fatalf("seedAgentConfigDir(dir source) error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dirTarget, "a.json")); err != nil || string(got) != "a" {
		t.Fatalf("seedAgentConfigDir(dir source) copy = (%q, %v)", string(got), err)
	}
	if _, err := os.Stat(filepath.Join(dirTarget, "subdir")); !os.IsNotExist(err) {
		t.Fatalf("seedAgentConfigDir(dir source) copied subdirectory unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirTarget, "link.json")); !os.IsNotExist(err) {
		t.Fatalf("seedAgentConfigDir(dir source) copied symlink unexpectedly: %v", err)
	}
}

func TestCopyFileErrorBranches(t *testing.T) {
	t.Parallel()

	if err := copyFile(filepath.Join(t.TempDir(), "missing.json"), filepath.Join(t.TempDir(), "out.json"), 0o600); err == nil {
		t.Fatal("copyFile(missing source) error = nil, want non-nil")
	}

	// MkdirAll failure: a path component of the target directory is a regular file.
	blockerDir := t.TempDir()
	blockerFile := filepath.Join(blockerDir, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocker) error = %v", err)
	}
	source := filepath.Join(t.TempDir(), "source.json")
	if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}
	target := filepath.Join(blockerFile, "sub", "out.json")
	if err := copyFile(source, target, 0o600); err == nil {
		t.Fatal("copyFile(blocked target dir) error = nil, want non-nil")
	}

	okTarget := filepath.Join(t.TempDir(), "nested", "out.json")
	if err := copyFile(source, okTarget, 0o600); err != nil {
		t.Fatalf("copyFile(ok) error = %v", err)
	}
	if got, err := os.ReadFile(okTarget); err != nil || string(got) != "content" {
		t.Fatalf("copyFile(ok) content = (%q, %v)", string(got), err)
	}
}

func TestPathContainsBranches(t *testing.T) {
	t.Parallel()

	if !pathContains("/a/b", "/a/b") {
		t.Fatal("pathContains(identical paths) = false, want true")
	}
	if !pathContains("/a", "/a/b/c") {
		t.Fatal("pathContains(parent of child) = false, want true")
	}
	if pathContains("/a/b/c", "/a") {
		t.Fatal("pathContains(child as parent) = true, want false")
	}
	if pathContains("/a/b", "/a/c") {
		t.Fatal("pathContains(siblings) = true, want false")
	}
	if pathContains("", "/a") {
		t.Fatal("pathContains(empty parent) = true, want false")
	}
	if pathContains("/a", "") {
		t.Fatal("pathContains(empty child) = true, want false")
	}
	// filepath.Rel errors when one path is absolute and the other relative.
	if pathContains("/a", "b") {
		t.Fatal("pathContains(abs parent, rel child) = true, want false")
	}
}

func TestValidateRuntimePromptImagesRejectsUnsupportedHarness(t *testing.T) {
	t.Parallel()

	images := []config.PromptImage{{Name: "a.png", DataBase64: "AA=="}}
	if err := validateRuntimePromptImages(agentruntime.Runtime{Harness: "unsupported-harness"}, images); err == nil {
		t.Fatal("validateRuntimePromptImages(unsupported harness) error = nil, want non-nil")
	}
	if err := validateRuntimePromptImages(agentruntime.Runtime{Harness: "unsupported-harness"}, nil); err != nil {
		t.Fatalf("validateRuntimePromptImages(no images) error = %v, want nil", err)
	}
	if err := validateRuntimePromptImages(agentruntime.Runtime{Harness: agentruntime.HarnessCodex}, images); err != nil {
		t.Fatalf("validateRuntimePromptImages(supported harness) error = %v, want nil", err)
	}
}

func TestNormalizeGitRemoteNameBranches(t *testing.T) {
	t.Parallel()

	if got := normalizeGitRemoteName(""); got != publishRemoteOrigin {
		t.Fatalf("normalizeGitRemoteName(empty) = %q, want %q", got, publishRemoteOrigin)
	}
	if got := normalizeGitRemoteName("   "); got != publishRemoteOrigin {
		t.Fatalf("normalizeGitRemoteName(blank) = %q, want %q", got, publishRemoteOrigin)
	}
	if got := normalizeGitRemoteName("upstream"); got != "upstream" {
		t.Fatalf("normalizeGitRemoteName(upstream) = %q, want upstream", got)
	}
}

func TestWithImplementationTargetGuardBlankPrompt(t *testing.T) {
	t.Parallel()

	if got := withImplementationTargetGuard("   \n\t "); got != "" {
		t.Fatalf("withImplementationTargetGuard(blank) = %q, want empty", got)
	}
	got := withImplementationTargetGuard("fix the bug")
	if !strings.Contains(got, "Agent input:\nfix the bug") {
		t.Fatalf("withImplementationTargetGuard(non-blank) missing agent input section: %q", got)
	}
}

func TestParseGitHubRepoRefEdgeCases(t *testing.T) {
	t.Parallel()

	if _, ok := parseGitHubRepoRef(""); ok {
		t.Fatal("parseGitHubRepoRef(empty) ok = true, want false")
	}
	if _, ok := parseGitHubRepoRef("https://github.com/"); ok {
		t.Fatal("parseGitHubRepoRef(no path) ok = true, want false")
	}
	if _, ok := parseGitHubRepoRef("https://github.com/onlyowner"); ok {
		t.Fatal("parseGitHubRepoRef(single segment path) ok = true, want false")
	}
	if _, ok := parseGitHubRepoRef("https://github.com/acme//x"); ok {
		t.Fatal("parseGitHubRepoRef(empty name segment) ok = true, want false")
	}
	if _, ok := parseGitHubRepoRef("https://github.com/acme/.git"); ok {
		t.Fatal("parseGitHubRepoRef(name trims to empty) ok = true, want false")
	}
	if _, ok := parseGitHubRepoRef("https://notgithub.example.com/acme/repo"); ok {
		t.Fatal("parseGitHubRepoRef(non-github host) ok = true, want false")
	}
	if _, ok := parseGitHubRepoRef("://not a url"); ok {
		t.Fatal("parseGitHubRepoRef(unparseable url) ok = true, want false")
	}
	ref, ok := parseGitHubRepoRef("https://github.com/acme/repo")
	if !ok || ref.hasGitSuffix {
		t.Fatalf("parseGitHubRepoRef(no .git suffix) = (%+v, %v), want hasGitSuffix=false", ref, ok)
	}
}

func TestGitHubRepoRefWithHTTPSOwnerBranches(t *testing.T) {
	t.Parallel()

	ref, ok := parseGitHubRepoRef("git@github.com:acme/repo.git")
	if !ok {
		t.Fatal("parseGitHubRepoRef() ok = false, want true")
	}
	if _, changed := ref.withHTTPSOwner(""); changed {
		t.Fatal("withHTTPSOwner(empty) changed = true, want false")
	}
	if _, changed := ref.withHTTPSOwner("acme"); changed {
		t.Fatal("withHTTPSOwner(same owner) changed = true, want false")
	}
	if _, changed := ref.withHTTPSOwner("new owner"); changed {
		t.Fatal("withHTTPSOwner(owner with space) changed = true, want false")
	}
	if _, changed := ref.withHTTPSOwner("new/owner"); changed {
		t.Fatal("withHTTPSOwner(owner with slash) changed = true, want false")
	}
	got, changed := ref.withHTTPSOwner("octocat")
	if !changed || got != "https://github.com/octocat/repo.git" {
		t.Fatalf("withHTTPSOwner(octocat) = (%q, %v), want https URL with .git suffix", got, changed)
	}

	noSuffixRef, ok := parseGitHubRepoRef("git@github.com:acme/repo")
	if !ok {
		t.Fatal("parseGitHubRepoRef(no suffix) ok = false, want true")
	}
	got, changed = noSuffixRef.withHTTPSOwner("octocat")
	if !changed || got != "https://github.com/octocat/repo" {
		t.Fatalf("withHTTPSOwner(no .git suffix) = (%q, %v)", got, changed)
	}
}

func TestIsRequiredStatusChecksNotConfiguredAdditionalBranches(t *testing.T) {
	t.Parallel()

	if isRequiredStatusChecksNotConfigured(execx.Result{}, nil) {
		t.Fatal("isRequiredStatusChecksNotConfigured(nil err) = true, want false")
	}
	if !isRequiredStatusChecksNotConfigured(execx.Result{Stderr: "required status checks are not enabled for this branch"}, errors.New("exit status 1")) {
		t.Fatal("isRequiredStatusChecksNotConfigured(not enabled) = false, want true")
	}
	if !isRequiredStatusChecksNotConfigured(execx.Result{Stderr: "required_status_checks not found"}, errors.New("exit status 1")) {
		t.Fatal("isRequiredStatusChecksNotConfigured(not found) = false, want true")
	}
	if !isRequiredStatusChecksNotConfigured(execx.Result{Stderr: "HTTP 404: required_status_checks missing"}, errors.New("exit status 1")) {
		t.Fatal("isRequiredStatusChecksNotConfigured(http 404 combined) = false, want true")
	}
	if isRequiredStatusChecksNotConfigured(execx.Result{Stderr: "some unrelated failure"}, errors.New("exit status 1")) {
		t.Fatal("isRequiredStatusChecksNotConfigured(unrelated) = true, want false")
	}
}

func TestReconcileNoChecksWithRequiredStatusChecks(t *testing.T) {
	t.Parallel()

	t.Run("no target repo", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		repo := repoWorkspace{Dir: "/repo", URL: "not-a-github-url"}
		noRequired, summary, err := h.reconcileNoChecksWithRequiredStatusChecks(context.Background(), repo)
		if err != nil || noRequired || summary != "" {
			t.Fatalf("reconcileNoChecksWithRequiredStatusChecks(no target repo) = (%v, %q, %v), want false,empty,nil", noRequired, summary, err)
		}
	})

	t.Run("no base branch", func(t *testing.T) {
		t.Parallel()
		h := New(&fakeRunner{t: t})
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git"}
		noRequired, summary, err := h.reconcileNoChecksWithRequiredStatusChecks(context.Background(), repo)
		if err != nil || noRequired || summary != "" {
			t.Fatalf("reconcileNoChecksWithRequiredStatusChecks(no base branch) = (%v, %q, %v), want false,empty,nil", noRequired, summary, err)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: requiredStatusChecksCommand(repo.Dir, "acme/repo", "main"), res: execx.Result{Stderr: "branch not protected"}, err: errors.New("exit status 1")},
		}}
		h := New(fake)
		noRequired, summary, err := h.reconcileNoChecksWithRequiredStatusChecks(context.Background(), repo)
		if err != nil || !noRequired || !strings.Contains(summary, "No required status checks") {
			t.Fatalf("reconcileNoChecksWithRequiredStatusChecks(not configured) = (%v, %q, %v)", noRequired, summary, err)
		}
	})

	t.Run("propagates other errors", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: requiredStatusChecksCommand(repo.Dir, "acme/repo", "main"), res: execx.Result{Stderr: "boom"}, err: errors.New("exit status 1: boom")},
		}}
		h := New(fake)
		_, _, err := h.reconcileNoChecksWithRequiredStatusChecks(context.Background(), repo)
		if err == nil {
			t.Fatal("reconcileNoChecksWithRequiredStatusChecks(other error) error = nil, want non-nil")
		}
	})

	t.Run("empty required checks list", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: requiredStatusChecksCommand(repo.Dir, "acme/repo", "main"), res: execx.Result{Stdout: `{"contexts":[],"checks":[]}`}},
		}}
		h := New(fake)
		noRequired, summary, err := h.reconcileNoChecksWithRequiredStatusChecks(context.Background(), repo)
		if err != nil || !noRequired || !strings.Contains(summary, "No required status checks") {
			t.Fatalf("reconcileNoChecksWithRequiredStatusChecks(empty list) = (%v, %q, %v)", noRequired, summary, err)
		}
	})

	t.Run("required checks present", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", URL: "git@github.com:acme/repo.git", BaseBranch: "main"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: requiredStatusChecksCommand(repo.Dir, "acme/repo", "main"), res: execx.Result{Stdout: `{"contexts":["ci/test"]}`}},
		}}
		h := New(fake)
		noRequired, summary, err := h.reconcileNoChecksWithRequiredStatusChecks(context.Background(), repo)
		if err != nil || noRequired || summary != "ci/test" {
			t.Fatalf("reconcileNoChecksWithRequiredStatusChecks(present) = (%v, %q, %v), want false,ci/test,nil", noRequired, summary, err)
		}
	})
}

func TestReconcileNoChecksWithWorkflowDispatch(t *testing.T) {
	t.Parallel()

	t.Run("head sha query fails", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), err: errors.New("boom")},
		}}
		h := New(fake)
		if _, _, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo); err == nil {
			t.Fatal("reconcileNoChecksWithWorkflowDispatch(head sha error) error = nil, want non-nil")
		}
	})

	t.Run("blank head sha", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "  "}},
		}}
		h := New(fake)
		reconciled, summary, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo)
		if err != nil || reconciled || summary != "" {
			t.Fatalf("reconcileNoChecksWithWorkflowDispatch(blank sha) = (%v, %q, %v)", reconciled, summary, err)
		}
	})

	t.Run("runs query fails", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "abc123"}},
			{cmd: workflowDispatchRunsCommand(repo.Dir, repo.Branch), err: errors.New("boom")},
		}}
		h := New(fake)
		if _, _, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo); err == nil {
			t.Fatal("reconcileNoChecksWithWorkflowDispatch(runs query error) error = nil, want non-nil")
		}
	})

	t.Run("blank runs output", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "abc123"}},
			{cmd: workflowDispatchRunsCommand(repo.Dir, repo.Branch), res: execx.Result{Stdout: "  "}},
		}}
		h := New(fake)
		reconciled, summary, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo)
		if err != nil || reconciled || summary != "" {
			t.Fatalf("reconcileNoChecksWithWorkflowDispatch(blank runs) = (%v, %q, %v)", reconciled, summary, err)
		}
	})

	t.Run("invalid runs json", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "abc123"}},
			{cmd: workflowDispatchRunsCommand(repo.Dir, repo.Branch), res: execx.Result{Stdout: "not json"}},
		}}
		h := New(fake)
		if _, _, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo); err == nil {
			t.Fatal("reconcileNoChecksWithWorkflowDispatch(invalid json) error = nil, want non-nil")
		}
	})

	t.Run("no matching run", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "abc123"}},
			{cmd: workflowDispatchRunsCommand(repo.Dir, repo.Branch), res: execx.Result{Stdout: `[{"headSha":"def456","status":"completed","conclusion":"success"}]`}},
		}}
		h := New(fake)
		reconciled, summary, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo)
		if err != nil || reconciled || summary != "" {
			t.Fatalf("reconcileNoChecksWithWorkflowDispatch(no match) = (%v, %q, %v)", reconciled, summary, err)
		}
	})

	t.Run("matching run passes", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "ABC123"}},
			{cmd: workflowDispatchRunsCommand(repo.Dir, repo.Branch), res: execx.Result{Stdout: `[{"headSha":"abc123","status":"completed","conclusion":"success","displayTitle":"CI"}]`}},
		}}
		h := New(fake)
		reconciled, summary, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo)
		if err != nil || !reconciled || !strings.Contains(summary, "pass") {
			t.Fatalf("reconcileNoChecksWithWorkflowDispatch(match pass) = (%v, %q, %v)", reconciled, summary, err)
		}
	})

	t.Run("matching run still pending", func(t *testing.T) {
		t.Parallel()
		repo := repoWorkspace{Dir: "/repo", Branch: "work"}
		fake := &fakeRunner{t: t, exps: []expectedRun{
			{cmd: headCommitSHACommand(repo.Dir), res: execx.Result{Stdout: "abc123"}},
			{cmd: workflowDispatchRunsCommand(repo.Dir, repo.Branch), res: execx.Result{Stdout: `[{"headSha":"abc123","status":"queued"}]`}},
		}}
		h := New(fake)
		reconciled, summary, err := h.reconcileNoChecksWithWorkflowDispatch(context.Background(), repo)
		if err != nil || reconciled || !strings.Contains(summary, "pending") {
			t.Fatalf("reconcileNoChecksWithWorkflowDispatch(pending) = (%v, %q, %v)", reconciled, summary, err)
		}
	})
}

func slicesEqualH2(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
