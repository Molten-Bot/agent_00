package failurefollowup

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestTaskLogDirAndIdentifierValidationBranches(t *testing.T) {
	t.Parallel()

	if got, ok := TaskLogDir("/tmp/logs", " - "); !ok || got != filepath.Join("/tmp/logs", "main") {
		t.Fatalf("TaskLogDir(fallback) = (%q, %v), want fallback path", got, ok)
	}
	if got, ok := TaskLogDir("/tmp/logs", "."); !ok || got != filepath.Join("/tmp/logs", "main") {
		t.Fatalf("TaskLogDir(dot fallback) = (%q, %v), want fallback path", got, ok)
	}

	paths := TaskLogPaths("/tmp/logs", "one---two")
	if len(paths) != 5 || paths[0] != filepath.Join("/tmp/logs", "one", "two") {
		t.Fatalf("TaskLogPaths(collapsed separators) = %#v", paths)
	}
	if paths[3] != filepath.Join("/tmp/logs", FallbackLogSubdir, LegacyTaskLogFileName) {
		t.Fatalf("TaskLogPaths(fallback legacy) = %#v, want fallback legacy path", paths)
	}
	if paths[4] != filepath.Join("/tmp/logs", FallbackLogSubdir, LogFileName) {
		t.Fatalf("TaskLogPaths(fallback current) = %#v, want fallback current path", paths)
	}
}

func TestNonRemediableRepoAccessReasonFallsBackToEmpty(t *testing.T) {
	t.Parallel()

	if got := NonRemediableRepoAccessReason(nil); got != "" {
		t.Fatalf("NonRemediableRepoAccessReason(nil) = %q, want empty", got)
	}
	if got := NonRemediableRepoAccessReason(errors.New("unrelated failure")); got != "" {
		t.Fatalf("NonRemediableRepoAccessReason(unrelated) = %q, want empty", got)
	}
	if got := NonRemediableRepoAccessReason(errors.New(" \n\t ")); got != "" {
		t.Fatalf("NonRemediableRepoAccessReason(empty error text) = %q, want empty", got)
	}
}

func TestNonRemediableFailureReasonRecognizesQuotaAndAllowsNoDelta(t *testing.T) {
	t.Parallel()

	if got := NonRemediableFailureReason(nil); got != "" {
		t.Fatalf("NonRemediableFailureReason(nil) = %q, want empty", got)
	}
	if got := NonRemediableFailureReason(errors.New(" \n\t ")); got != "" {
		t.Fatalf("NonRemediableFailureReason(empty error text) = %q, want empty", got)
	}
	if got := NonRemediableFailureReason(errors.New("codex: ERROR: Quota exceeded. Check your plan and billing details.")); got != "quota exceeded" {
		t.Fatalf("NonRemediableFailureReason(quota) = %q, want %q", got, "quota exceeded")
	}
	if got := NonRemediableFailureReason(errors.New("codex: codex reported failure: Failure: Live Claude task could not create PR. Error details: `Your organization has disabled Claude subscription access for Claude Code · Use an Anthropic API key instead, or ask your admin to enable access`")); got != "organization has disabled claude subscription access" {
		t.Fatalf("NonRemediableFailureReason(Claude subscription disabled) = %q, want %q", got, "organization has disabled claude subscription access")
	}
	if got := NonRemediableFailureReason(errors.New("codex: codex reported failure: Failure: user-portal changes not applied. Error details: sandbox rejected writes to `/home/jef/git/moltenbot/user-portal`: `writing outside of the project; rejected by user approval settings`.")); got != "sandbox rejected writes to" {
		t.Fatalf("NonRemediableFailureReason(sandbox write rejection) = %q, want %q", got, "sandbox rejected writes to")
	}
	if got := NonRemediableFailureReason(errors.New("codex: codex reported failure: Failure: Full site/assets download not completed. Error details: shell network blocked. `wget` failed DNS for `www.kaanawaveco.com`; `curl --resolve` failed connect to port 443. Browser tool crawled public pages, but cannot save binary assets.")); got != "shell network blocked" {
		t.Fatalf("NonRemediableFailureReason(shell network blocked) = %q, want %q", got, "shell network blocked")
	}
	if got := NonRemediableFailureReason(errors.New("clone: run git [clone --single-branch git@github.com:Molten-Bot/forum.git /workspace/repo]: exit status 128 (fatal: unable to access 'https://github.com/Molten-Bot/forum.git/': Could not resolve host: github.com)")); got != "could not resolve host" {
		t.Fatalf("NonRemediableFailureReason(clone DNS failure) = %q, want %q", got, "could not resolve host")
	}
	if got := NonRemediableFailureReason(errors.New("codex: codex reported failure: Failure: No June 14 release entry created. Error details: `git-changes-by-day` ran across all repos. `2026-06-14` UTC rows: `0` in every repo. Adding entry would require invented project/note content.")); got != "would require invented" {
		t.Fatalf("NonRemediableFailureReason(invented content) = %q, want %q", got, "would require invented")
	}
	if got := NonRemediableFailureReason(errors.New("task failed because this branch has no delta from `main`; No commits between main and moltenhub-fix")); got != "" {
		t.Fatalf("NonRemediableFailureReason(no delta) = %q, want empty", got)
	}
	if got := NonRemediableFailureReason(errors.New("claude: claude reported failure: Failure: task premise false, source file no exist. Error details: `site/how-it-works.html` not in repo. Can't fabricate page content from nothing.")); got != "task premise false" {
		t.Fatalf("NonRemediableFailureReason(task premise false) = %q, want %q", got, "task premise false")
	}
	if got := NonRemediableFailureReason(errors.New("codex: codex reported failure: Failure: cannot add pricing copy. Error details: no source pricing content exists in the repository. Cannot fabricate pricing figures.")); got != "cannot fabricate" {
		t.Fatalf("NonRemediableFailureReason(cannot fabricate) = %q, want %q", got, "cannot fabricate")
	}
}
