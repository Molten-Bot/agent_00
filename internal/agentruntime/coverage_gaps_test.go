package agentruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- claude_auth.go ---

func TestClaudeAuthenticationFailureRecognizesProviderDiagnostics(t *testing.T) {
	t.Parallel()

	for _, message := range []string{
		"Failed to authenticate. Please run /login again.",
		"  FAILED TO AUTHENTICATE.  ",
		"API error: 401 Unauthorized",
		"intro line\napi error: 401\nmore context",
		"OAuth access token has expired. Refresh and retry.",
		"Invalid API key · Please run /login",
	} {
		if !ClaudeAuthenticationFailure(message) {
			t.Errorf("ClaudeAuthenticationFailure(%q) = false, want true", message)
		}
	}

	for _, message := range []string{
		"",
		"build succeeded",
		"MCP server: HTTP 401 Unauthorized", // not a prefix match
		"please re-authenticate later",
	} {
		if ClaudeAuthenticationFailure(message) {
			t.Errorf("ClaudeAuthenticationFailure(%q) = true, want false", message)
		}
	}
}

// --- codex_session.go ---

func TestCodexHomeFallsBackToProcessEnvironWhenEmpty(t *testing.T) {
	// Mutates process environment via t.Setenv; cannot run in parallel.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	got, err := CodexHome(nil)
	if err != nil {
		t.Fatalf("CodexHome(nil) error = %v", err)
	}
	want := filepath.Join(home, ".codex")
	if got != want {
		t.Fatalf("CodexHome(nil) = %q, want %q", got, want)
	}
}

func TestAcquireCodexSessionDefaultsNilContext(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	session, err := AcquireCodexSession(nil, home)
	if err != nil {
		t.Fatalf("AcquireCodexSession(nil, ...) error = %v", err)
	}
	session.Release()
}

func TestAcquireCodexSessionMkdirAllFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(blocker, "codex-home")
	_, err := AcquireCodexSession(context.Background(), home)
	if err == nil || !strings.Contains(err.Error(), "prepare persistent Codex home") {
		t.Fatalf("AcquireCodexSession() error = %v, want prepare persistent Codex home failure", err)
	}
}

func TestAcquireCodexSessionOpenFileFailure(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// Pre-create the lock file path as a directory so opening it as RDWR fails.
	if err := os.Mkdir(filepath.Join(home, ".agent00-session.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := AcquireCodexSession(context.Background(), home)
	if err == nil || !strings.Contains(err.Error(), "open Codex session lock") {
		t.Fatalf("AcquireCodexSession() error = %v, want open Codex session lock failure", err)
	}
}

func TestCodexAuthFingerprintMissingAuthFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	got, err := codexAuthFingerprint(home)
	if err != nil {
		t.Fatalf("codexAuthFingerprint() error = %v", err)
	}
	if got != "keyring-or-missing" {
		t.Fatalf("codexAuthFingerprint() = %q, want keyring-or-missing", got)
	}
}

func TestCodexAuthFingerprintReadError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "auth.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := codexAuthFingerprint(home)
	if err == nil || !strings.Contains(err.Error(), "read Codex credential generation") {
		t.Fatalf("codexAuthFingerprint() error = %v, want read failure", err)
	}
}

func TestCheckCodexAuthenticationNoMarkerFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := CheckCodexAuthentication(home); err != nil {
		t.Fatalf("CheckCodexAuthentication() error = %v, want nil when no marker exists", err)
	}
}

func TestCheckCodexAuthenticationMarkerReadError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// A directory at the marker path forces a non-ErrNotExist read failure.
	if err := os.Mkdir(filepath.Join(home, codexAuthFailureFile), 0o700); err != nil {
		t.Fatal(err)
	}
	err := CheckCodexAuthentication(home)
	if err == nil || !strings.Contains(err.Error(), "read Codex authentication state") {
		t.Fatalf("CheckCodexAuthentication() error = %v, want read Codex authentication state failure", err)
	}
	if errors.Is(err, ErrCodexAuthRequired) {
		t.Fatal("CheckCodexAuthentication() misclassified a read failure as auth required")
	}
}

func TestCheckCodexAuthenticationFingerprintError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, codexAuthFailureFile), []byte("some-fingerprint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "auth.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := CheckCodexAuthentication(home)
	if err == nil || !strings.Contains(err.Error(), "read Codex credential generation") {
		t.Fatalf("CheckCodexAuthentication() error = %v, want fingerprint failure propagated", err)
	}
}

func TestMarkCodexAuthenticationRequiredFingerprintError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "auth.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := MarkCodexAuthenticationRequired(home)
	if err == nil || !strings.Contains(err.Error(), "read Codex credential generation") {
		t.Fatalf("MarkCodexAuthenticationRequired() error = %v, want fingerprint failure propagated", err)
	}
}

func TestMarkCodexAuthenticationRequiredCreateTempError(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "missing-home")
	err := MarkCodexAuthenticationRequired(home)
	if err == nil || !strings.Contains(err.Error(), "record Codex authentication failure") {
		t.Fatalf("MarkCodexAuthenticationRequired() error = %v, want record failure", err)
	}
}

func TestClearCodexAuthenticationRequiredMissingMarker(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := ClearCodexAuthenticationRequired(home); err != nil {
		t.Fatalf("ClearCodexAuthenticationRequired() error = %v, want nil when no marker exists", err)
	}
}

// --- runtime.go ---

func TestResolveSandboxPathRejectsDanglingSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "nonexistent-target"), link); err != nil {
		t.Fatal(err)
	}
	_, err := resolveSandboxPath(link)
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("resolveSandboxPath() error = %v, want dangling symlink error", err)
	}
}

func TestResolveSandboxPathPropagatesDanglingSymlinkInAncestor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "nonexistent-target"), link); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(link, "child")
	_, err := resolveSandboxPath(nested)
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("resolveSandboxPath() error = %v, want propagated dangling symlink error", err)
	}
}

// chdirIntoRemovedDir changes the process working directory to a directory
// that is then deleted, so that filepath.Abs on a relative path fails because
// os.Getwd fails. It restores the original working directory on cleanup.
// Must not run in parallel with other tests: it mutates global process state.
func chdirIntoRemovedDir(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSandboxPathReturnsAbsErrorWhenCwdIsGone(t *testing.T) {
	// Uses chdirIntoRemovedDir, which mutates the process working directory.
	chdirIntoRemovedDir(t)
	if _, err := resolveSandboxPath("relative-dir"); err == nil {
		t.Fatal("resolveSandboxPath() error = nil, want error when cwd is unresolvable")
	}
}

func TestResolveSandboxPathPropagatesNonNotExistEvalSymlinksError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	_, err := resolveSandboxPath(a)
	if err == nil {
		t.Fatal("resolveSandboxPath() error = nil, want symlink loop error")
	}
	if strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("resolveSandboxPath() error = %v, want raw EvalSymlinks failure, not dangling symlink", err)
	}
}

func TestValidateWorkspaceDirsAbsErrorsWhenCwdIsGone(t *testing.T) {
	// Uses chdirIntoRemovedDir, which mutates the process working directory.
	chdirIntoRemovedDir(t)

	err := validateWorkspaceDirs("ignored-target", RunOptions{WorkspaceDir: "relative-workspace"})
	if err == nil || !strings.Contains(err.Error(), "resolve agent workspace") {
		t.Fatalf("validateWorkspaceDirs() error = %v, want resolve agent workspace failure", err)
	}

	// An absolute workspace resolves without touching the (now-gone) cwd, so
	// the failure below comes from resolving the relative target directory.
	knownAbsoluteWorkspace := os.TempDir()
	err = validateWorkspaceDirs("relative-target", RunOptions{WorkspaceDir: knownAbsoluteWorkspace})
	if err == nil || !strings.Contains(err.Error(), "resolve agent directory") {
		t.Fatalf("validateWorkspaceDirs() error = %v, want resolve agent directory failure", err)
	}
}

func TestValidateWorkspaceDirsSkipsBlankDirectoryEntries(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	// Direct call bypasses BuildCommand's own blank-dir filtering, so this
	// exercises validateWorkspaceDirs' own skip-blank-entry loop logic.
	err := validateWorkspaceDirs("  ", RunOptions{WorkspaceDir: root, WritableDirs: []string{" ", sub}})
	if err != nil {
		t.Fatalf("validateWorkspaceDirs() error = %v, want nil when blank entries are skipped", err)
	}
}

func TestValidatePromptImageSupportSkipsCheckWhenNoImages(t *testing.T) {
	t.Parallel()
	if err := validatePromptImageSupport("totally-unsupported-harness", []string{"  ", ""}); err != nil {
		t.Fatalf("validatePromptImageSupport() error = %v, want nil when no images are attached", err)
	}
}

func TestSupportedPromptImageHarnessLabelsJoinsThreeOrMore(t *testing.T) {
	originalNames := harnessDisplayNames
	originalHarnesses := promptImageHarnesses
	harnessDisplayNames = map[string]string{
		"alpha": "Alpha",
		"beta":  "Beta",
		"gamma": "Gamma",
	}
	promptImageHarnesses = map[string]struct{}{
		"alpha": {},
		"beta":  {},
		"gamma": {},
	}
	t.Cleanup(func() {
		harnessDisplayNames = originalNames
		promptImageHarnesses = originalHarnesses
	})

	got := supportedPromptImageHarnessLabels()
	want := "Alpha, Beta, or Gamma"
	if got != want {
		t.Fatalf("supportedPromptImageHarnessLabels() = %q, want %q", got, want)
	}
}

func TestSupportedPromptImageHarnessLabelsSkipsDuplicateLabelsAmongMultiple(t *testing.T) {
	originalNames := harnessDisplayNames
	originalHarnesses := promptImageHarnesses
	harnessDisplayNames = map[string]string{
		"delta":   "Zed",
		"epsilon": "Zed", // duplicate label, must be skipped
		"zeta":    "Omega",
	}
	promptImageHarnesses = map[string]struct{}{
		"delta":   {},
		"epsilon": {},
		"zeta":    {},
	}
	t.Cleanup(func() {
		harnessDisplayNames = originalNames
		promptImageHarnesses = originalHarnesses
	})

	got := supportedPromptImageHarnessLabels()
	want := "Zed or Omega"
	if got != want {
		t.Fatalf("supportedPromptImageHarnessLabels() = %q, want %q", got, want)
	}
}

func TestBuildCommandClaudeReadOnlyWithWorkspaceDirGrantsIODirWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	cmd, err := rt.BuildCommand(repo, "review", RunOptions{
		WorkspaceDir:    root,
		DisallowedTools: []string{"Write"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	ioDir := filepath.Join(root, ".agent00-agent-io")
	wantFragment := `,"` + ioDir + `"="write"`
	found := false
	for _, arg := range cmd.Args {
		if strings.Contains(arg, wantFragment) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Args = %#v, want IO directory write permission %q", cmd.Args, wantFragment)
	}
}

func TestBuildCommandCodexReadOnlySandboxForDisallowedTools(t *testing.T) {
	t.Parallel()
	rt, err := Resolve(HarnessCodex, "")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	cmd, err := rt.BuildCommand("/tmp/repo", "review", RunOptions{
		DisallowedTools: []string{"Write"},
		WritableDirs:    []string{"/tmp/should-be-ignored"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	if !containsString(cmd.Args, "read-only") {
		t.Fatalf("Args = %#v, want read-only sandbox", cmd.Args)
	}
	for _, arg := range cmd.Args {
		if arg == "--add-dir" || strings.Contains(arg, "writable_roots") {
			t.Fatalf("read-only Codex invocation must not grant writable dirs: %#v", cmd.Args)
		}
	}
}

func TestBuildCommandRejectsDanglingWorkspaceSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "dangling-workspace")
	if err := os.Symlink(filepath.Join(root, "nonexistent"), link); err != nil {
		t.Fatal(err)
	}
	rt, _ := Resolve(HarnessCodex, "")
	_, err := rt.BuildCommand("/tmp/repo", "fix", RunOptions{WorkspaceDir: link})
	if err == nil || !strings.Contains(err.Error(), "resolve agent workspace") {
		t.Fatalf("BuildCommand() error = %v, want resolve agent workspace failure", err)
	}
}

func TestBuildCommandRejectsDanglingTargetDirSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "dangling-target")
	if err := os.Symlink(filepath.Join(root, "nonexistent"), link); err != nil {
		t.Fatal(err)
	}
	rt, _ := Resolve(HarnessCodex, "")
	_, err := rt.BuildCommand(link, "fix", RunOptions{WorkspaceDir: root})
	if err == nil || !strings.Contains(err.Error(), "resolve agent working directory") {
		t.Fatalf("BuildCommand() error = %v, want resolve agent working directory failure", err)
	}
}

func TestBuildCommandRejectsDanglingWritableDirSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "dangling-writable")
	if err := os.Symlink(filepath.Join(root, "nonexistent"), link); err != nil {
		t.Fatal(err)
	}
	rt, _ := Resolve(HarnessCodex, "")
	_, err := rt.BuildCommand(repo, "fix", RunOptions{WorkspaceDir: root, WritableDirs: []string{link}})
	if err == nil || !strings.Contains(err.Error(), "resolve writable agent directory") {
		t.Fatalf("BuildCommand() error = %v, want resolve writable agent directory failure", err)
	}
}

func TestBuildCommandClaudeFallsBackToProcessEnvironWhenEnvEmpty(t *testing.T) {
	// Reads the live process environment, so it must not run concurrently
	// with tests that mutate it via t.Setenv.
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := Resolve(HarnessClaude, "")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	cmd, err := rt.BuildCommand(repo, "fix", RunOptions{WorkspaceDir: root})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	wantCodexHome := filepath.Join(root, ".agent00-agent-io", "config", "sandbox")
	if !containsString(cmd.Env, "CODEX_HOME="+wantCodexHome) {
		t.Fatalf("Env = %#v, want task-local CODEX_HOME entry", cmd.Env)
	}
	if len(cmd.Env) < 2 {
		t.Fatalf("Env = %#v, want process environment plus task-local CODEX_HOME", cmd.Env)
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "CODEX_HOME=") && entry != "CODEX_HOME="+wantCodexHome {
			t.Fatalf("Env retained a stale CODEX_HOME entry: %#v", cmd.Env)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
