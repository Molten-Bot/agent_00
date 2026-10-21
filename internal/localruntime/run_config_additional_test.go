package localruntime

import (
	"strings"
	"testing"
)

func TestNormalizeRunConfigMapAndAliasesValidation(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeRunConfigMap(`{"repo":"git@github.com:acme/repo.git","branch":"release","prompt":"do work"}`)
	if err != nil {
		t.Fatalf("normalizeRunConfigMap(string) error = %v", err)
	}
	if got, want := stringAt(normalized, "baseBranch"), "release"; got != want {
		t.Fatalf("baseBranch alias = %q, want %q", got, want)
	}

	if _, err := normalizeRunConfigMap(`["not","an","object"]`); err == nil {
		t.Fatal("normalizeRunConfigMap(array JSON) error = nil, want non-nil")
	}
	if _, err := normalizeRunConfigMap(42); err == nil {
		t.Fatal("normalizeRunConfigMap(non-map) error = nil, want non-nil")
	}
	if _, err := normalizeRunConfigMap(""); err == nil {
		t.Fatal("normalizeRunConfigMap(empty string) error = nil, want non-nil")
	}
	if err := normalizeRunConfigAliases(nil); err == nil {
		t.Fatal("normalizeRunConfigAliases(nil) error = nil, want non-nil")
	}

	err = normalizeRunConfigAliases(map[string]any{
		"prompt":          "x",
		"libraryTaskName": "unit-test-coverage",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot include both prompt and librarytaskname") {
		t.Fatalf("normalizeRunConfigAliases(conflict) error = %v", err)
	}
}

func TestExpandLibraryTaskRunConfigBranches(t *testing.T) {
	t.Parallel()

	expanded, err := expandLibraryTaskRunConfig(map[string]any{
		"libraryTaskName": "unit-test-coverage",
		"repos":           []any{"git@github.com:acme/repo.git"},
		"targetSubdir":    "cmd/harness",
		"responseMode":    "off",
		"reviewers":       []any{"octocat"},
		"extra":           "kept",
	}, "unit-test-coverage")
	if err != nil {
		t.Fatalf("expandLibraryTaskRunConfig() error = %v", err)
	}
	if got := stringAt(expanded, "extra"); got != "kept" {
		t.Fatalf("expanded extra = %q, want kept", got)
	}
	if got := nonEmptyStringArray(expanded["repos"]); len(got) != 1 || got[0] != "git@github.com:acme/repo.git" {
		t.Fatalf("expanded repos = %#v, want original repo list", expanded["repos"])
	}
	if got := stringAt(expanded, "targetSubdir"); got != "cmd/harness" {
		t.Fatalf("expanded targetSubdir = %q, want caller override", got)
	}
	if got := stringAt(expanded, "responseMode"); got != "off" {
		t.Fatalf("expanded responseMode = %q, want caller override", got)
	}
	if got := nonEmptyStringArray(expanded["reviewers"]); len(got) != 1 || got[0] != "octocat" {
		t.Fatalf("expanded reviewers = %#v, want caller override", expanded["reviewers"])
	}

	if _, err := expandLibraryTaskRunConfig(map[string]any{
		"repo": "git@github.com:acme/repo.git",
	}, "missing-task"); err == nil {
		t.Fatal("expandLibraryTaskRunConfig(missing task) error = nil, want non-nil")
	}
}
