package customlibrary

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/library"
)

// newTestRepo creates a local git repository with the given files already
// committed, so Syncer.Sync can clone it exactly like a real remote.
func newTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "commit")
	return dir
}

func commitTestRepo(t *testing.T, dir, path, content, message string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", message)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestSyncerSyncClonesAndGeneratesCatalog(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t, map[string]string{
		"prompt/fix-bug.md": "---\ndisplayName: Fix Bug\nicon: bug\n---\nFind and fix the bug.\n",
	})
	root := t.TempDir()
	syncer := Syncer{Root: root, Runner: execx.OSRunner{}}

	result, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: repo})
	if err != nil {
		t.Fatalf("Sync() error: %v", err)
	}
	if result.TaskCount != 1 {
		t.Fatalf("TaskCount = %d, want 1", result.TaskCount)
	}
	if result.CommitSHA == "" {
		t.Fatalf("CommitSHA is empty")
	}
	if !result.Changed {
		t.Fatalf("Changed = false on first sync, want true")
	}

	catalog, err := library.LoadCatalog(syncer.GeneratedDir("acme"))
	if err != nil {
		t.Fatalf("LoadCatalog(generated) error: %v", err)
	}
	task, ok := catalog.Task("acme--fix-bug")
	if !ok {
		t.Fatalf("catalog missing task acme--fix-bug; got %v", catalog.Names())
	}
	if task.DisplayName != "Fix Bug" || task.Prompt != "Find and fix the bug." {
		t.Fatalf("unexpected task %+v", task)
	}
}

func TestSyncerSyncIsIdempotentWhenUnchanged(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t, map[string]string{
		"prompt/task.md": "---\nname: task\n---\nDo the thing.\n",
	})
	root := t.TempDir()
	syncer := Syncer{Root: root, Runner: execx.OSRunner{}}
	ctx := context.Background()

	if _, err := syncer.Sync(ctx, Source{Name: "acme", Repo: repo}); err != nil {
		t.Fatalf("first Sync() error: %v", err)
	}
	result, err := syncer.Sync(ctx, Source{Name: "acme", Repo: repo})
	if err != nil {
		t.Fatalf("second Sync() error: %v", err)
	}
	if result.Changed {
		t.Fatalf("Changed = true on unchanged repo, want false")
	}
}

func TestSyncerSyncPicksUpNewCommits(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t, map[string]string{
		"prompt/task.md": "---\nname: task\n---\nOriginal prompt.\n",
	})
	root := t.TempDir()
	syncer := Syncer{Root: root, Runner: execx.OSRunner{}}
	ctx := context.Background()

	if _, err := syncer.Sync(ctx, Source{Name: "acme", Repo: repo}); err != nil {
		t.Fatalf("first Sync() error: %v", err)
	}

	commitTestRepo(t, repo, "prompt/task.md", "---\nname: task\n---\nUpdated prompt.\n", "update")

	result, err := syncer.Sync(ctx, Source{Name: "acme", Repo: repo})
	if err != nil {
		t.Fatalf("second Sync() error: %v", err)
	}
	if !result.Changed {
		t.Fatalf("Changed = false after new commit, want true")
	}

	catalog, err := library.LoadCatalog(syncer.GeneratedDir("acme"))
	if err != nil {
		t.Fatalf("LoadCatalog(generated) error: %v", err)
	}
	task, ok := catalog.Task("acme--task")
	if !ok || task.Prompt != "Updated prompt." {
		t.Fatalf("catalog not updated: ok=%v task=%+v", ok, task)
	}
}

func TestSyncerSyncKeepsLastGoodCatalogOnBadMarkdown(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t, map[string]string{
		"prompt/task.md": "---\nname: task\n---\nGood prompt.\n",
	})
	root := t.TempDir()
	syncer := Syncer{Root: root, Runner: execx.OSRunner{}}
	ctx := context.Background()

	if _, err := syncer.Sync(ctx, Source{Name: "acme", Repo: repo}); err != nil {
		t.Fatalf("first Sync() error: %v", err)
	}

	// An empty prompt body is invalid; the broken commit must not clobber the
	// previously generated catalog.
	commitTestRepo(t, repo, "prompt/task.md", "---\nname: task\n---\n\n", "break it")

	if _, err := syncer.Sync(ctx, Source{Name: "acme", Repo: repo}); err == nil {
		t.Fatalf("Sync() = nil error, want error for empty prompt body")
	}

	catalog, err := library.LoadCatalog(syncer.GeneratedDir("acme"))
	if err != nil {
		t.Fatalf("LoadCatalog(generated) error after failed sync: %v", err)
	}
	task, ok := catalog.Task("acme--task")
	if !ok || task.Prompt != "Good prompt." {
		t.Fatalf("expected last-good catalog to survive, got ok=%v task=%+v", ok, task)
	}
}

func TestSyncerRemoveSource(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t, map[string]string{
		"prompt/task.md": "---\nname: task\n---\nDo it.\n",
	})
	root := t.TempDir()
	syncer := Syncer{Root: root, Runner: execx.OSRunner{}}
	ctx := context.Background()

	if _, err := syncer.Sync(ctx, Source{Name: "acme", Repo: repo}); err != nil {
		t.Fatalf("Sync() error: %v", err)
	}
	if err := syncer.RemoveSource("acme"); err != nil {
		t.Fatalf("RemoveSource() error: %v", err)
	}
	if _, err := os.Stat(syncer.sourceDir("acme")); !os.IsNotExist(err) {
		t.Fatalf("source directory still exists after RemoveSource: err=%v", err)
	}
}

func TestSyncerSyncRejectsInvalidSource(t *testing.T) {
	t.Parallel()

	syncer := Syncer{Root: t.TempDir(), Runner: execx.OSRunner{}}
	if _, err := syncer.Sync(context.Background(), Source{Name: "", Repo: "whatever"}); err == nil {
		t.Fatalf("Sync() = nil error, want validation error")
	}
}
