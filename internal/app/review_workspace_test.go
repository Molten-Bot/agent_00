package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestReviewPassesAllocateDiskWorkspaceBeforeClone(t *testing.T) {
	root := t.TempDir()
	ram, disk := filepath.Join(root, "ram"), filepath.Join(root, "disk")
	t.Setenv("HARNESS_WORKSPACE_RAM_BASE", ram)
	t.Setenv("HARNESS_WORKSPACE_DISK_BASE", disk)
	t.Setenv("HARNESS_WORKSPACE_ROOT_NAME", "tasks")
	for _, tc := range []struct {
		name     string
		review   *config.ReviewConfig
		passes   int
		pinned   bool
		wantBase string
	}{
		{name: "implementation retains RAM", wantBase: ram},
		{name: "PR review", review: &config.ReviewConfig{PRNumber: 1}, wantBase: disk},
		{name: "post-task review", passes: 1, wantBase: disk},
		{name: "durable issue root remains pinned", passes: 1, pinned: true, wantBase: filepath.Join(root, "durable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				if cmd.Name == "git" && len(cmd.Args) > 0 && cmd.Args[0] == "clone" {
					return execx.Result{}, errors.New("stop after workspace allocation")
				}
				return execx.Result{}, nil
			}))
			h.FinalReviewPasses = tc.passes
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			h.Workspace.PathExists = func(string) bool { return true }
			h.Workspace.CanExec = func(string) bool { return true }
			h.Workspace.CheckSpace = nil
			h.Workspace.NewGUID = func() string { return "test-run" }
			h.Workspace.ReadFile = func(string) ([]byte, error) { return []byte("test instructions"), nil }
			h.Workspace.MkdirAll = os.MkdirAll
			want := filepath.Join(tc.wantBase, "tasks", "test-run")
			if tc.pinned {
				h.Workspace.Root = tc.wantBase
				want = filepath.Join(tc.wantBase, "test-run")
			}
			result := h.Run(context.Background(), config.Config{RepoURL: "https://github.com/example/repo.git", Prompt: "Review this change", Review: tc.review})
			if result.ExitCode != ExitClone {
				t.Fatalf("exit = %d: %v", result.ExitCode, result.Err)
			}
			if result.WorkspaceDir != want {
				t.Fatalf("workspace = %q, want %q", result.WorkspaceDir, want)
			}
		})
	}
}
