package app

import (
	"context"
	"errors"
	"strings"
	"testing"

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
