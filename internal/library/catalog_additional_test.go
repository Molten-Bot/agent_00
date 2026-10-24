package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandRunConfigErrorsForUnknownTaskAndMissingRepo(t *testing.T) {
	t.Parallel()

	catalog := Catalog{
		byName: map[string]TaskDefinition{
			"known": {Name: "known", Prompt: "do work"},
		},
	}

	if _, err := catalog.ExpandRunConfig("missing", "git@github.com:acme/repo.git", "main"); err == nil {
		t.Fatal("ExpandRunConfig(unknown) error = nil, want non-nil")
	}
	if _, err := catalog.ExpandRunConfig("known", "", "main"); err == nil {
		t.Fatal("ExpandRunConfig(empty repo) error = nil, want non-nil")
	}
}

func TestExpandRunConfigDefaultsMissingTargetSubdir(t *testing.T) {
	t.Parallel()

	catalog := Catalog{
		byName: map[string]TaskDefinition{
			"code-review": {Name: "code-review", Prompt: "review the pull request"},
		},
	}

	cfg, err := catalog.ExpandRunConfig("code-review", "git@github.com:acme/repo.git", "main")
	if err != nil {
		t.Fatalf("ExpandRunConfig() error = %v", err)
	}
	if got, want := cfg.TargetSubdir, "."; got != want {
		t.Fatalf("TargetSubdir = %q, want %q", got, want)
	}
}

func TestExpandRunConfigRequiresConfiguredBaseBranch(t *testing.T) {
	t.Parallel()

	catalog := Catalog{
		byName: map[string]TaskDefinition{
			"fix-merge-main": {
				Name:                     "fix-merge-main",
				Prompt:                   "merge main into the current branch",
				RequiresNonDefaultBranch: true,
			},
		},
	}

	if _, err := catalog.ExpandRunConfig("fix-merge-main", "git@github.com:acme/repo.git", ""); err == nil {
		t.Fatal("ExpandRunConfig(empty branch) error = nil, want non-nil")
	} else if !strings.Contains(err.Error(), `library task "fix-merge-main" requires a non-default base branch`) {
		t.Fatalf("ExpandRunConfig(empty branch) error = %v", err)
	}

	cfg, err := catalog.ExpandRunConfig("fix-merge-main", "git@github.com:acme/repo.git", "feature/conflicted")
	if err != nil {
		t.Fatalf("ExpandRunConfig(feature branch) error = %v", err)
	}
	if got, want := cfg.BaseBranch, "feature/conflicted"; got != want {
		t.Fatalf("BaseBranch = %q, want %q", got, want)
	}
}

func TestExpandRunConfigWithContextAppendsPromptContext(t *testing.T) {
	t.Parallel()

	catalog := Catalog{
		byName: map[string]TaskDefinition{
			"root-cause-bug-fix": {Name: "root-cause-bug-fix", Prompt: "Fix the bug.", PromptContextLabel: "Bug details"},
		},
	}

	cfg, err := catalog.ExpandRunConfigWithContext("root-cause-bug-fix", "git@github.com:acme/repo.git", "main", "  Login throws a 500.  ")
	if err != nil {
		t.Fatalf("ExpandRunConfigWithContext() error = %v", err)
	}
	if want := "Fix the bug.\n\nAdditional context from requester:\nLogin throws a 500."; cfg.Prompt != want {
		t.Fatalf("Prompt = %q, want %q", cfg.Prompt, want)
	}

	cfg, err = catalog.ExpandRunConfigWithContext("root-cause-bug-fix", "git@github.com:acme/repo.git", "main", "")
	if err != nil {
		t.Fatalf("ExpandRunConfigWithContext(empty context) error = %v", err)
	}
	if cfg.Prompt != "Fix the bug." {
		t.Fatalf("Prompt = %q, want unchanged task prompt", cfg.Prompt)
	}
}

func TestExpandRunConfigWithContextRequiresContextWhenTaskDemandsIt(t *testing.T) {
	t.Parallel()

	catalog := Catalog{
		byName: map[string]TaskDefinition{
			"needs-context": {
				Name:                  "needs-context",
				Prompt:                "Do the thing.",
				PromptContextLabel:    "Target URL",
				PromptContextRequired: true,
			},
		},
	}

	if _, err := catalog.ExpandRunConfigWithContext("needs-context", "git@github.com:acme/repo.git", "main", "  "); err == nil {
		t.Fatal("ExpandRunConfigWithContext(blank context) error = nil, want non-nil")
	} else if !strings.Contains(err.Error(), `library task "needs-context" requires additional prompt context`) {
		t.Fatalf("ExpandRunConfigWithContext(blank context) error = %v", err)
	}

	cfg, err := catalog.ExpandRunConfigWithContext("needs-context", "git@github.com:acme/repo.git", "main", "https://example.com")
	if err != nil {
		t.Fatalf("ExpandRunConfigWithContext(with context) error = %v", err)
	}
	if !strings.Contains(cfg.Prompt, "https://example.com") {
		t.Fatalf("Prompt = %q, want to contain supplied context", cfg.Prompt)
	}
}

func TestDecodeTaskDefinitionValidatesPromptContextFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "task.json")

	if _, err := decodeTaskDefinition(path, "", []byte(`{"name":"x","prompt":"p","promptContextRequired":true}`)); err == nil {
		t.Fatal("decodeTaskDefinition(required without label) error = nil, want non-nil")
	} else if !strings.Contains(err.Error(), "promptContextLabel is required when promptContextRequired is true") {
		t.Fatalf("decodeTaskDefinition(required without label) error = %v", err)
	}

	if _, err := decodeTaskDefinition(path, "", []byte(`{"name":"x","prompt":"p","promptContextPlaceholder":"e.g. a URL"}`)); err == nil {
		t.Fatal("decodeTaskDefinition(placeholder without label) error = nil, want non-nil")
	} else if !strings.Contains(err.Error(), "promptContextPlaceholder requires promptContextLabel") {
		t.Fatalf("decodeTaskDefinition(placeholder without label) error = %v", err)
	}

	task, err := decodeTaskDefinition(path, "", []byte(`{"name":"x","prompt":"p","promptContextLabel":"Details","promptContextPlaceholder":"e.g. a URL","promptContextRequired":true}`))
	if err != nil {
		t.Fatalf("decodeTaskDefinition(valid) error = %v", err)
	}
	if task.PromptContextLabel != "Details" || task.PromptContextPlaceholder != "e.g. a URL" || !task.PromptContextRequired {
		t.Fatalf("decoded task = %+v, want promptContext fields preserved", task)
	}
}

func TestLoadTaskDefinitionsRejectsEmptyFileObject(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := loadTaskDefinitions(path)
	if err == nil {
		t.Fatal("loadTaskDefinitions(empty) error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "at least one task is required") {
		t.Fatalf("loadTaskDefinitions(empty) error = %v", err)
	}
}

func TestResolveCatalogDirAndHelpersEdgeCases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if got := resolveCatalogDir(dir); got != dir {
		t.Fatalf("resolveCatalogDir(abs) = %q, want %q", got, dir)
	}

	nonCatalog := filepath.Join(dir, "not-catalog")
	if err := os.MkdirAll(nonCatalog, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if isCatalogDir(nonCatalog) {
		t.Fatalf("isCatalogDir(%q) = true, want false", nonCatalog)
	}
	if _, ok := findDirUpward("", "library"); ok {
		t.Fatal("findDirUpward(empty startDir) ok = true, want false")
	}
	if _, ok := findDirUpward(nonCatalog, ""); ok {
		t.Fatal("findDirUpward(empty relPath) ok = true, want false")
	}
}

func TestResolveCatalogDirKeepsExistingRelativeCatalogPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%q) error = %v", tmp, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	rel := "catalog-data"
	catalogDir := filepath.Join(tmp, rel)
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(catalog) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "task.json"), []byte(`{"name":"x","prompt":"p"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(task) error = %v", err)
	}

	if got := resolveCatalogDir(rel); got != rel {
		t.Fatalf("resolveCatalogDir(existing relative) = %q, want %q", got, rel)
	}
}

func TestResolveCatalogDirUsesConfiguredLibraryDirEnv(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%q) error = %v", tmp, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	catalogDir := filepath.Join(t.TempDir(), "container-library")
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(catalogDir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "task.json"), []byte(`{"name":"x","prompt":"p"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(task) error = %v", err)
	}
	t.Setenv(catalogDirEnv, catalogDir)

	if got := resolveCatalogDir(DefaultDir); got != catalogDir {
		t.Fatalf("resolveCatalogDir(DefaultDir) = %q, want %q", got, catalogDir)
	}
}

func TestResolveCatalogDirUsesAgentsSeedEnvFallback(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%q) error = %v", tmp, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	catalogDir := filepath.Join(t.TempDir(), "seeded-library")
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(catalogDir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "AGENTS.md"), []byte("# seed"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "task.json"), []byte(`{"name":"x","prompt":"p"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(task) error = %v", err)
	}
	t.Setenv(agentsSeedEnv, filepath.Join(catalogDir, "AGENTS.md"))

	if got := resolveCatalogDir(DefaultDir); got != catalogDir {
		t.Fatalf("resolveCatalogDir(DefaultDir) = %q, want %q", got, catalogDir)
	}
}
