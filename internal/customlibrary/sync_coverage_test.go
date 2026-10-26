package customlibrary

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/library"
)

// gitCallHandler answers one subprocess invocation in place of real git.
type gitCallHandler func(cmd execx.Command) (execx.Result, error)

// scriptedGitRunner is a fake execx.Runner keyed by git subcommand (the
// first positional arg, e.g. "fetch", "reset", "clone", "rev-parse") so
// Syncer.Sync's git-command branches can be exercised deterministically,
// without ever invoking a real git subprocess. A subcommand with no
// registered handler succeeds with a fixed stdout value. "rev-parse" calls
// are counted (Sync issues one before any mutation and one after), so
// handlers may be registered as "rev-parse#1" / "rev-parse#2" to target one
// specific call.
type scriptedGitRunner struct {
	t        *testing.T
	handlers map[string]gitCallHandler

	mu            sync.Mutex
	calls         []execx.Command
	revParseCalls int
}

func (r *scriptedGitRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	r.t.Helper()
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	r.mu.Unlock()

	if cmd.Name != "git" || len(cmd.Args) == 0 {
		r.t.Fatalf("unexpected command: %+v", cmd)
	}
	sub := cmd.Args[0]

	if sub == "rev-parse" {
		r.mu.Lock()
		r.revParseCalls++
		n := r.revParseCalls
		r.mu.Unlock()
		if h, ok := r.handlers[fmt.Sprintf("rev-parse#%d", n)]; ok {
			return h(cmd)
		}
	}
	if h, ok := r.handlers[sub]; ok {
		return h(cmd)
	}
	return execx.Result{Stdout: "deadbeefcafef00d\n"}, nil
}

func okHandler(stdout string) gitCallHandler {
	return func(execx.Command) (execx.Result, error) { return execx.Result{Stdout: stdout}, nil }
}

func errHandler(message string) gitCallHandler {
	return func(execx.Command) (execx.Result, error) { return execx.Result{}, errors.New(message) }
}

// markExistingRepo creates just enough on disk (a ".git" marker) for
// Syncer.Sync to treat repoDir as an already-cloned repo and take the
// fetch+reset branch instead of the first-time clone branch.
func markExistingRepo(t *testing.T, repoDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git marker: %v", err)
	}
}

func TestSyncerSyncFetchFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := &scriptedGitRunner{t: t, handlers: map[string]gitCallHandler{
		"fetch": errHandler("network unreachable"),
	}}
	syncer := Syncer{Root: root, Runner: runner}
	markExistingRepo(t, syncer.repoDir("acme"))

	_, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: "https://example.invalid/repo.git"})
	if err == nil || !strings.Contains(err.Error(), "fetch acme") {
		t.Fatalf("Sync() error = %v, want fetch acme error", err)
	}
}

func TestSyncerSyncResetFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := &scriptedGitRunner{t: t, handlers: map[string]gitCallHandler{
		"reset": errHandler("hard reset failed"),
	}}
	syncer := Syncer{Root: root, Runner: runner}
	markExistingRepo(t, syncer.repoDir("acme"))

	_, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: "https://example.invalid/repo.git"})
	if err == nil || !strings.Contains(err.Error(), "reset acme") {
		t.Fatalf("Sync() error = %v, want reset acme error", err)
	}
}

func TestSyncerSyncCloneFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := &scriptedGitRunner{t: t, handlers: map[string]gitCallHandler{
		"clone": errHandler("repository not found"),
	}}
	syncer := Syncer{Root: root, Runner: runner}
	// No ".git" marker: repoDir does not exist yet, so Sync takes the
	// first-time clone branch.

	_, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: "https://example.invalid/repo.git"})
	if err == nil || !strings.Contains(err.Error(), "clone acme") {
		t.Fatalf("Sync() error = %v, want clone acme error", err)
	}
}

func TestSyncerSyncResolveHeadAfterCloneFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := &scriptedGitRunner{t: t, handlers: map[string]gitCallHandler{
		// rev-parse#1 is the "beforeSHA" probe (its error is discarded by
		// Sync); rev-parse#2 is the post-clone HEAD resolution whose error
		// must fail Sync.
		"rev-parse#2": errHandler("HEAD unresolvable"),
	}}
	syncer := Syncer{Root: root, Runner: runner}

	_, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: "https://example.invalid/repo.git"})
	if err == nil || !strings.Contains(err.Error(), "resolve HEAD for acme") {
		t.Fatalf("Sync() error = %v, want resolve HEAD for acme error", err)
	}
}

func TestSyncerSyncPrepareSourceDirFailure(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	// Root points at a regular file, so MkdirAll(sourceDir) can never
	// succeed: a path component exists and is not a directory.
	rootFile := filepath.Join(parent, "rootfile")
	if err := os.WriteFile(rootFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write root file: %v", err)
	}
	runner := &scriptedGitRunner{t: t}
	syncer := Syncer{Root: rootFile, Runner: runner}

	_, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: "https://example.invalid/repo.git"})
	if err == nil || !strings.Contains(err.Error(), "prepare acme") {
		t.Fatalf("Sync() error = %v, want prepare acme error", err)
	}
}

func TestSyncerSyncResetWorkspaceFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := &scriptedGitRunner{t: t}
	syncer := Syncer{Root: root, Runner: runner}

	sourceDir := syncer.sourceDir("acme")
	repoDir := syncer.repoDir("acme")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "stale.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	// No ".git" marker, so Sync takes the first-time clone branch, which
	// first clears repoDir. Making sourceDir read-only (after repoDir
	// already has content) blocks exactly that removal, without blocking
	// the preceding MkdirAll(sourceDir) (a no-op since it already exists).
	if err := os.Chmod(sourceDir, 0o500); err != nil {
		t.Fatalf("chmod sourceDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceDir, 0o700) })

	_, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: "https://example.invalid/repo.git"})
	if err == nil || !strings.Contains(err.Error(), "reset workspace for acme") {
		t.Fatalf("Sync() error = %v, want reset workspace for acme error", err)
	}
}

func TestSyncerSyncWriteGeneratedCatalogFailureIsWrapped(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := &scriptedGitRunner{t: t}
	syncer := Syncer{Root: root, Runner: runner}

	repoDir := syncer.repoDir("acme")
	markExistingRepo(t, repoDir)
	if err := os.MkdirAll(filepath.Join(repoDir, "prompt"), 0o700); err != nil {
		t.Fatalf("mkdir prompt dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "prompt", "task.md"), []byte("---\nname: task\n---\nDo it.\n"), 0o600); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}

	// fetch+reset succeed (default handler) and loadPromptMarkdown succeeds,
	// but sourceDir is read-only, so writeGeneratedCatalog's own staging
	// directory can never be created. Sync must wrap that failure with its
	// own source-name prefix.
	sourceDir := syncer.sourceDir("acme")
	if err := os.Chmod(sourceDir, 0o500); err != nil {
		t.Fatalf("chmod sourceDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceDir, 0o700) })

	result, err := syncer.Sync(context.Background(), Source{Name: "acme", Repo: "https://example.invalid/repo.git"})
	if err == nil {
		t.Fatalf("Sync() = nil error, want staging directory failure")
	}
	if !strings.HasPrefix(err.Error(), "acme: ") {
		t.Fatalf("Sync() error = %v, want it to be wrapped with the source name", err)
	}
	if !strings.Contains(err.Error(), "create staging directory") {
		t.Fatalf("Sync() error = %v, want create staging directory failure", err)
	}
	if result != (SyncResult{}) {
		t.Fatalf("Sync() result = %+v, want zero value on error", result)
	}
}

// --- runner()/root() default-vs-configured branches ------------------------

func TestSyncerRunnerDefaultsToOSRunner(t *testing.T) {
	t.Parallel()

	syncer := Syncer{}
	if _, ok := syncer.runner().(execx.OSRunner); !ok {
		t.Fatalf("runner() = %T, want execx.OSRunner", syncer.runner())
	}
}

func TestSyncerRunnerUsesConfiguredRunner(t *testing.T) {
	t.Parallel()

	custom := &scriptedGitRunner{t: t}
	syncer := Syncer{Runner: custom}
	got, ok := syncer.runner().(*scriptedGitRunner)
	if !ok || got != custom {
		t.Fatalf("runner() = %#v, want the configured custom runner", syncer.runner())
	}
}

func TestSyncerRootDefaultsWhenEmptyOrWhitespace(t *testing.T) {
	t.Parallel()

	for _, root := range []string{"", "   ", "\t\n"} {
		syncer := Syncer{Root: root}
		if got := syncer.root(); got != DefaultRoot {
			t.Fatalf("root() with Root=%q = %q, want %q", root, got, DefaultRoot)
		}
	}
}

func TestSyncerRootUsesConfiguredValue(t *testing.T) {
	t.Parallel()

	syncer := Syncer{Root: "/custom/library-root"}
	if got := syncer.root(); got != "/custom/library-root" {
		t.Fatalf("root() = %q, want /custom/library-root", got)
	}
}

// --- RemoveSource edge cases -------------------------------------------

func TestSyncerRemoveSourceEmptyOrWhitespaceNameIsNoop(t *testing.T) {
	t.Parallel()

	syncer := Syncer{Root: t.TempDir()}
	for _, name := range []string{"", "   ", "\t"} {
		if err := syncer.RemoveSource(name); err != nil {
			t.Fatalf("RemoveSource(%q) error = %v, want nil", name, err)
		}
	}
}

func TestSyncerRemoveSourceNormalizesCase(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	syncer := Syncer{Root: root}
	if err := os.MkdirAll(syncer.sourceDir("acme"), 0o700); err != nil {
		t.Fatalf("mkdir sourceDir: %v", err)
	}

	if err := syncer.RemoveSource("  ACME  "); err != nil {
		t.Fatalf("RemoveSource() error: %v", err)
	}
	if _, err := os.Stat(syncer.sourceDir("acme")); !os.IsNotExist(err) {
		t.Fatalf("sourceDir still exists after case-insensitive RemoveSource: err=%v", err)
	}
}

func TestSyncerRemoveSourceNonexistentIsNoop(t *testing.T) {
	t.Parallel()

	syncer := Syncer{Root: t.TempDir()}
	if err := syncer.RemoveSource("never-existed"); err != nil {
		t.Fatalf("RemoveSource() error = %v, want nil for a source that was never synced", err)
	}
}

// --- writeGeneratedCatalog direct edge cases -----------------------------

func validTask(name string) library.TaskDefinition {
	return library.TaskDefinition{Name: name, DisplayName: "Task", Prompt: "Do the thing."}
}

func TestWriteGeneratedCatalogClearStagingDirectoryFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	name := "acme"
	// sourceDir (root/name) is a regular file, not a directory, so any
	// attempt to touch root/name/generated.tmp fails to even traverse.
	if err := os.WriteFile(filepath.Join(root, name), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write file in place of sourceDir: %v", err)
	}
	syncer := Syncer{Root: root}

	err := syncer.writeGeneratedCatalog(name, []library.TaskDefinition{validTask("acme--task")})
	if err == nil || !strings.Contains(err.Error(), "clear staging directory") {
		t.Fatalf("writeGeneratedCatalog() error = %v, want clear staging directory failure", err)
	}
}

func TestWriteGeneratedCatalogCreateStagingDirectoryFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	name := "acme"
	syncer := Syncer{Root: root}
	sourceDir := syncer.sourceDir(name)
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatalf("mkdir sourceDir: %v", err)
	}
	if err := os.Chmod(sourceDir, 0o500); err != nil {
		t.Fatalf("chmod sourceDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceDir, 0o700) })

	err := syncer.writeGeneratedCatalog(name, []library.TaskDefinition{validTask("acme--task")})
	if err == nil || !strings.Contains(err.Error(), "create staging directory") {
		t.Fatalf("writeGeneratedCatalog() error = %v, want create staging directory failure", err)
	}
}

func TestWriteGeneratedCatalogWriteTaskFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	syncer := Syncer{Root: root}

	// A task name containing a path separator makes its target
	// "<tmpDir>/<name>.json" reference a nonexistent intermediate
	// directory, so the write itself must fail.
	err := syncer.writeGeneratedCatalog("acme", []library.TaskDefinition{validTask("acme--a/b")})
	if err == nil || !strings.Contains(err.Error(), "write task") {
		t.Fatalf("writeGeneratedCatalog() error = %v, want write task failure", err)
	}
}

func TestWriteGeneratedCatalogValidationFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	syncer := Syncer{Root: root}

	// An empty task Name is accepted by writeGeneratedCatalog's own loop
	// (it only json-encodes and writes the file), but the catalog loader it
	// re-validates through rejects a task with no name.
	err := syncer.writeGeneratedCatalog("acme", []library.TaskDefinition{{Name: "", Prompt: "Do it."}})
	if err == nil || !strings.Contains(err.Error(), "validate generated catalog") {
		t.Fatalf("writeGeneratedCatalog() error = %v, want validate generated catalog failure", err)
	}
}

func TestWriteGeneratedCatalogClearPreviousCatalogFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	name := "acme"
	syncer := Syncer{Root: root}
	genDir := syncer.generatedDir(name)
	if err := os.MkdirAll(genDir, 0o700); err != nil {
		t.Fatalf("mkdir genDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "old.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write stale catalog file: %v", err)
	}
	// Only genDir (not its parent sourceDir) is made read-only, so the new
	// staging directory can still be created and validated; only removing
	// the previous generated catalog's contents must fail.
	if err := os.Chmod(genDir, 0o500); err != nil {
		t.Fatalf("chmod genDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(genDir, 0o700) })

	err := syncer.writeGeneratedCatalog(name, []library.TaskDefinition{validTask("acme--task")})
	if err == nil || !strings.Contains(err.Error(), "clear previous catalog") {
		t.Fatalf("writeGeneratedCatalog() error = %v, want clear previous catalog failure", err)
	}
}
