package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCustomSourcesRootDefaultsWhenEnvUnset covers the default-path branch of
// CustomSourcesRoot when HARNESS_LIBRARY_CUSTOM_DIR is unset/blank.
func TestCustomSourcesRootDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv(CustomSourcesDirEnv, "")

	if got, want := CustomSourcesRoot(), defaultCustomSourcesRoot; got != want {
		t.Fatalf("CustomSourcesRoot() = %q, want default %q", got, want)
	}
}

// TestCustomSourcesRootHonorsEnvOverride covers the override branch of
// CustomSourcesRoot, including trimming surrounding whitespace, per
// AGENTS.md's documented HARNESS_LIBRARY_CUSTOM_DIR override behavior.
func TestCustomSourcesRootHonorsEnvOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "company-sources")
	t.Setenv(CustomSourcesDirEnv, "  "+override+"  ")

	if got, want := CustomSourcesRoot(), override; got != want {
		t.Fatalf("CustomSourcesRoot() = %q, want trimmed override %q", got, want)
	}
}

// TestLoadCustomizedCatalogPropagatesBuiltinLoadError covers the
// includeDefault error branch of loadCustomizedCatalog: when the built-in
// LoadCatalog(dir) call fails, LoadCustomizedCatalog must return that error
// rather than silently continuing to merge custom sources.
func TestLoadCustomizedCatalogPropagatesBuiltinLoadError(t *testing.T) {
	t.Setenv(catalogDirEnv, "")
	t.Setenv(agentsSeedEnv, "")
	t.Setenv(CustomSourcesDirEnv, filepath.Join(t.TempDir(), "unused"))

	missingDir := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := LoadCustomizedCatalog(missingDir)
	if err == nil {
		t.Fatal("LoadCustomizedCatalog(missing builtin dir) error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "read library dir") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadCustomizedCatalogSkipsCustomSourceWithInvalidJSON covers the
// per-source LoadCatalog error branch inside loadCustomizedCatalog's merge
// loop: a custom source whose generated directory exists and looks like a
// catalog dir (has a *.json file) but fails to parse must be skipped rather
// than failing the whole catalog, per LoadCustomizedCatalog's documented
// "skipped rather than failing" contract for a broken/mid-write source.
func TestLoadCustomizedCatalogSkipsCustomSourceWithInvalidJSON(t *testing.T) {
	t.Setenv(catalogDirEnv, "")
	t.Setenv(agentsSeedEnv, "")

	dir := t.TempDir()
	writeGeneratedTask(t, dir, "only-builtin", "Prompt.")

	customRoot := t.TempDir()
	generatedDir := filepath.Join(customRoot, "acme", "generated")
	if err := os.MkdirAll(generatedDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Malformed JSON: isCatalogDir only checks for a *.json file, so this
	// source is attempted but LoadCatalog must fail to parse it.
	if err := os.WriteFile(filepath.Join(generatedDir, "broken.json"), []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("write broken task: %v", err)
	}
	t.Setenv(CustomSourcesDirEnv, customRoot)

	catalog, err := LoadCustomizedCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCustomizedCatalog() error: %v", err)
	}
	if len(catalog.Tasks) != 1 {
		t.Fatalf("Tasks = %+v, want exactly the built-in task (broken source skipped)", catalog.Tasks)
	}
	if _, ok := catalog.Task("only-builtin"); !ok {
		t.Fatalf("missing built-in task; got %v", catalog.Names())
	}
}
