package library

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGeneratedTask(t *testing.T, dir, name, prompt string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := `{"` + name + `": {"prompt": "` + prompt + `"}}`
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write task %s: %v", name, err)
	}
}

func TestLoadCustomizedCatalogMergesCustomSources(t *testing.T) {
	t.Setenv(catalogDirEnv, "")
	t.Setenv(agentsSeedEnv, "")

	dir := t.TempDir()
	writeGeneratedTask(t, dir, "code-review-custom", "Do a custom review.")

	customRoot := t.TempDir()
	t.Setenv(CustomSourcesDirEnv, customRoot)
	writeGeneratedTask(t, filepath.Join(customRoot, "acme", "generated"), "acme--fix-bug", "Fix the bug.")

	catalog, err := LoadCustomizedCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCustomizedCatalog() error: %v", err)
	}
	if _, ok := catalog.Task("code-review-custom"); !ok {
		t.Fatalf("missing built-in task; got %v", catalog.Names())
	}
	if task, ok := catalog.Task("acme--fix-bug"); !ok || task.Prompt != "Fix the bug." {
		t.Fatalf("missing/incorrect custom task: ok=%v task=%+v", ok, task)
	}
}

func TestLoadCustomizedCatalogIgnoresMissingCustomRoot(t *testing.T) {
	t.Setenv(catalogDirEnv, "")
	t.Setenv(agentsSeedEnv, "")

	dir := t.TempDir()
	writeGeneratedTask(t, dir, "only-builtin", "Prompt.")

	t.Setenv(CustomSourcesDirEnv, filepath.Join(t.TempDir(), "does-not-exist"))

	catalog, err := LoadCustomizedCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCustomizedCatalog() error: %v", err)
	}
	if len(catalog.Tasks) != 1 {
		t.Fatalf("Tasks = %+v, want exactly the built-in task", catalog.Tasks)
	}
}

func TestLoadCustomizedCatalogSkipsUnsyncedSource(t *testing.T) {
	t.Setenv(catalogDirEnv, "")
	t.Setenv(agentsSeedEnv, "")

	dir := t.TempDir()
	writeGeneratedTask(t, dir, "only-builtin", "Prompt.")

	customRoot := t.TempDir()
	// A source directory exists but has never synced (no "generated" subdir).
	if err := os.MkdirAll(filepath.Join(customRoot, "acme"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv(CustomSourcesDirEnv, customRoot)

	catalog, err := LoadCustomizedCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCustomizedCatalog() error: %v", err)
	}
	if len(catalog.Tasks) != 1 {
		t.Fatalf("Tasks = %+v, want exactly the built-in task", catalog.Tasks)
	}
}

func TestLoadCustomizedCatalogSkipsNameCollisionWithBuiltin(t *testing.T) {
	t.Setenv(catalogDirEnv, "")
	t.Setenv(agentsSeedEnv, "")

	dir := t.TempDir()
	writeGeneratedTask(t, dir, "shared-name", "Builtin prompt.")

	customRoot := t.TempDir()
	t.Setenv(CustomSourcesDirEnv, customRoot)
	writeGeneratedTask(t, filepath.Join(customRoot, "acme", "generated"), "shared-name", "Custom prompt.")

	catalog, err := LoadCustomizedCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCustomizedCatalog() error: %v", err)
	}
	task, ok := catalog.Task("shared-name")
	if !ok || task.Prompt != "Builtin prompt." {
		t.Fatalf("expected built-in task to win on collision, got ok=%v task=%+v", ok, task)
	}
	if len(catalog.Tasks) != 1 {
		t.Fatalf("Tasks = %+v, want exactly one task after collision skip", catalog.Tasks)
	}
}
