package app

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

type diskFullRunner struct {
	res execx.Result
	err error
}

func (r diskFullRunner) Run(_ context.Context, _ execx.Command) (execx.Result, error) {
	return r.res, r.err
}

func TestRunCommandUnchecked_ClassifiesDiskSpaceExhaustion(t *testing.T) {
	t.Parallel()
	underlying := errors.New("exit status 128")
	cases := []struct {
		name   string
		stderr string
	}{
		{"git index lock write error", "fatal: sha1 file '/x/.git/index.lock' write error. Out of diskspace"},
		{"posix no space left", "fatal: write error: No space left on device"},
		{"disk quota", "fatal: unable to write new_index file: Disk quota exceeded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := Harness{
				Logf:   func(string, ...any) {},
				Runner: diskFullRunner{res: execx.Result{Stderr: tc.stderr}, err: underlying},
			}
			_, err := h.runCommandUnchecked(context.Background(), "git", execx.Command{Name: "git", Dir: "/mnt/agent_00/tasks/x/repo", Args: []string{"add", "-A"}})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "ran out of space") {
				t.Fatalf("error = %q, want actionable disk-space message", err.Error())
			}
			if !strings.Contains(err.Error(), "HARNESS_WORKSPACE_DISK_BASE") {
				t.Fatalf("error = %q, want remediation hint", err.Error())
			}
			if !errors.Is(err, underlying) {
				t.Fatalf("error = %v, want to wrap underlying error %v", err, underlying)
			}
		})
	}
}

func TestRunCommandUnchecked_LeavesUnrelatedErrorsUntouched(t *testing.T) {
	t.Parallel()
	underlying := errors.New("exit status 1")
	h := Harness{
		Logf:   func(string, ...any) {},
		Runner: diskFullRunner{res: execx.Result{Stderr: "fatal: not a git repository"}, err: underlying},
	}
	_, err := h.runCommandUnchecked(context.Background(), "git", execx.Command{Name: "git", Dir: "/repo", Args: []string{"status"}})
	if !errors.Is(err, underlying) || err != underlying {
		t.Fatalf("error = %v, want untouched underlying error %v", err, underlying)
	}
}

func TestRunCommandUnchecked_DoesNotClassifyNonGitQuotaErrors(t *testing.T) {
	t.Parallel()
	underlying := errors.New("exit status 1")
	h := Harness{
		Logf:   func(string, ...any) {},
		Runner: diskFullRunner{res: execx.Result{Stderr: "disk quota exceeded for this billing plan"}, err: underlying},
	}
	_, err := h.runCommandUnchecked(context.Background(), "checks", execx.Command{Name: "gh", Dir: "/repo", Args: []string{"api", "repos/x/y"}})
	if !errors.Is(err, underlying) || err != underlying {
		t.Fatalf("error = %v, want untouched underlying error %v, not relabeled as disk exhaustion", err, underlying)
	}
}

func TestRunCodexRejectsAgentDiskSpaceExhaustionBeforeValidationGap(t *testing.T) {
	t.Parallel()

	targetDir := t.TempDir()
	prompt := "improve API test coverage to 90%"
	firstCmd := codexCommand(targetDir, prompt)
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{
			cmd: firstCmd,
			res: execx.Result{
				Stdout: "Failure: Cannot safely meet coverage target in current runtime.\nError details: `bun run test:unit:coverage` failed: `vitest: command not found`. Installing vitest failed with ENOSPC while copying files from cache.",
			},
		},
	}}

	h := New(fake)
	err := h.runCodex(context.Background(), agentruntime.Default(), targetDir, prompt, codexRunOptions{}, "", "")
	if err == nil {
		t.Fatal("runCodex() error = nil, want workspace storage exhaustion")
	}
	if !strings.Contains(err.Error(), "workspace storage ran out of space") {
		t.Fatalf("runCodex() error = %q, want workspace storage diagnosis", err)
	}
	if !errors.Is(err, workspace.ErrDiskFull) {
		t.Fatalf("runCodex() error = %v, want typed disk exhaustion", err)
	}
	if !strings.Contains(err.Error(), "HARNESS_WORKSPACE_DISK_BASE") {
		t.Fatalf("runCodex() error = %q, want storage remediation hint", err)
	}
}

func TestRunCodexClassifiesDiskFullOnNonzeroExit(t *testing.T) {
	t.Parallel()
	underlying := errors.New("exit status 1")
	for _, tc := range []struct {
		name, stdout, stderr string
		prompt               string
		err                  error
		wantDiskFull         bool
		wantFailure          string
	}{
		{name: "terminal diagnostic", stderr: "ENOSPC: failed writing cache", err: underlying, wantDiskFull: true},
		{name: "terminal failure report", stdout: "Failure: Validation failed.\nError details: copy failed: no space left on device", wantDiskFull: true},
		{name: "terminal report overrides trailing stderr diagnostic", stdout: "Failure: Tests failed.\nError details: assertion failed.", stderr: "ENOSPC: failed writing cache", wantFailure: "Failure: Tests failed. Error details: assertion failed."},
		{name: "terminal summary overrides trailing stderr diagnostic", stdout: "Failure: Tests failed.", stderr: "Error: write ENOSPC", wantFailure: "Failure: Tests failed."},
		{name: "typed error overrides terminal report", stdout: "Failure: Tests failed.", err: syscall.ENOSPC, wantDiskFull: true},
		{name: "narration before error details", stdout: "Failure: Validation failed.\nValidation: Local checks stopped before reaching the assertions.\n**Error details:** copy failed with ENOSPC", wantDiskFull: true},
		{name: "compact stderr narration before error details", stderr: "Failure: Validation failed.\nLocal checks stopped before reaching the assertions.\nError details: copy failed with ENOSPC", wantDiskFull: true},
		{name: "narration alone mentions disk full", stdout: "Failure: Tests failed.\nRecovered an earlier ENOSPC by moving the cache."},
		{name: "narration quotes earlier disk full report", stdout: "Failure: Tests failed.\nRecovered from this earlier error:\n~~~text\nError details: copy failed with ENOSPC\n~~~\nError details: assertion failed."},
		{name: "details across transcript boundary", stdout: "Failure: Tests failed.\nexec\nError details: copy failed with ENOSPC"},
		{name: "split failure report", stdout: "Failure: Validation failed.", stderr: "Error details: copy failed with ENOSPC", wantDiskFull: true},
		{name: "split failure report with bullet", stdout: "Failure: Validation failed.", stderr: "**Error details:**\n- copy failed with ENOSPC", wantDiskFull: true},
		{name: "split details echoed from prompt", stdout: "Failure: Validation failed.", stderr: "Error details: copy failed with ENOSPC", prompt: "Error details: copy failed with ENOSPC", wantFailure: "Failure: Validation failed."},
		{name: "later stdout failure", stdout: "Failure: ENOSPC copying cache.\nError details: copy failed with ENOSPC\nFailure: Tests failed.\nError details: assertion failed.", wantFailure: "Failure: Tests failed. Error details: assertion failed."},
		{name: "later compact stderr failure", stderr: "Failure: ENOSPC copying cache.\nError details: copy failed with ENOSPC\nFailure: Tests failed.\nError details: assertion failed.", wantFailure: "Failure: Tests failed. Error details: assertion failed."},
		{name: "later disk full failure", stdout: "Failure: Tests failed.\nError details: assertion failed.\nFailure: Validation could not finish.\nError details: copy failed with ENOSPC", wantDiskFull: true},
		{name: "typed error", err: syscall.ENOSPC, wantDiskFull: true},
		{name: "prompt echo", stderr: "task prompt: investigate ENOSPC\nERROR: unrelated failure", err: underlying},
		{name: "echoed failure field", stderr: "user\nFailure: ENOSPC\nERROR: unrelated failure", prompt: "Failure: ENOSPC"},
		{name: "diagnostic shares prompt wording", stderr: "Error: write ENOSPC", prompt: "ENOSPC", err: underlying, wantDiskFull: true},
		{name: "recovered tool failure", stdout: "ENOSPC: failed writing cache\nRecovered by moving cache.\nFailure: unrelated test failed."},
		{name: "stale error details", stdout: "Error details: ENOSPC: failed writing cache\nRecovered by moving cache.\nFailure: unrelated test failed.\nError details: assertion failed."},
		{name: "recovered stderr diagnostic", stderr: "ENOSPC: failed writing cache\nERROR: unrelated failure", err: underlying},
		{name: "successful run with disk warning", stdout: "Done.", stderr: "ENOSPC: failed writing cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(diskFullRunner{err: tc.err, res: execx.Result{Stdout: tc.stdout, Stderr: tc.stderr}})
			prompt := tc.prompt
			if prompt == "" {
				prompt = "investigate ENOSPC"
			}
			_, err := h.runCodexWithHeartbeat(context.Background(), agentruntime.Default(), t.TempDir(), prompt, codexRunOptions{}, agentInvocationLogMetadata{})
			if tc.err != nil && !errors.Is(err, tc.err) || errors.Is(err, workspace.ErrDiskFull) != tc.wantDiskFull {
				t.Fatalf("agent error = %v, want disk exhaustion=%t and underlying %v", err, tc.wantDiskFull, tc.err)
			}
			if tc.wantFailure != "" && (err == nil || !strings.Contains(err.Error(), tc.wantFailure)) {
				t.Fatalf("agent error = %v, want %q", err, tc.wantFailure)
			}
		})
	}
}

func TestEnsureWorkspaceSpaceForCommit(t *testing.T) {
	t.Parallel()
	repo := &repoWorkspace{URL: "https://example.com/org/repo.git", Dir: "/mnt/agent_00/tasks/x/repo"}

	t.Run("no checker configured", func(t *testing.T) {
		t.Parallel()
		h := Harness{Workspace: workspace.Manager{}}
		if err := h.ensureWorkspaceSpaceForCommit(repo); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("space available", func(t *testing.T) {
		t.Parallel()
		h := Harness{Workspace: workspace.Manager{CheckSpace: func(string) error { return nil }}}
		if err := h.ensureWorkspaceSpaceForCommit(repo); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("space exhausted", func(t *testing.T) {
		t.Parallel()
		checkErr := errors.New("workspace storage /mnt/agent_00/tasks/x has only 3 MiB free")
		var checkedPath string
		h := Harness{Workspace: workspace.Manager{CheckSpace: func(p string) error {
			checkedPath = p
			return checkErr
		}}}
		err := h.ensureWorkspaceSpaceForCommit(repo)
		if err == nil {
			t.Fatal("expected an error")
		}
		if checkedPath != repo.Dir {
			t.Fatalf("checked path = %q, want %q", checkedPath, repo.Dir)
		}
		if !errors.Is(err, checkErr) {
			t.Fatalf("error = %v, want to wrap %v", err, checkErr)
		}
		if !strings.Contains(err.Error(), repo.URL) {
			t.Fatalf("error = %q, want to mention repo URL %q", err.Error(), repo.URL)
		}
	})
}
