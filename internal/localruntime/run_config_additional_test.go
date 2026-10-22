package localruntime

import (
	"encoding/json"
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

func TestPositiveIntValue(t *testing.T) {
	t.Parallel()

	bigJSONNumber := json.Number("12")
	badJSONNumber := json.Number("not-a-number")

	tests := []struct {
		name     string
		value    any
		wantInt  int
		wantBool bool
	}{
		{"positive int", int(5), 5, true},
		{"zero int", int(0), 0, false},
		{"negative int", int(-3), -3, false},
		{"positive int32", int32(7), 7, true},
		{"non-positive int32", int32(0), 0, false},
		{"positive int64", int64(9), 9, true},
		{"non-positive int64", int64(-1), -1, false},
		{"whole positive float64", float64(4), 4, true},
		{"fractional float64", float64(4.5), 4, false},
		{"non-positive float64", float64(-2), -2, false},
		{"positive json.Number", bigJSONNumber, 12, true},
		{"malformed json.Number", badJSONNumber, 0, false},
		{"string", "5", 0, false},
		{"nil", nil, 0, false},
		{"bool", true, 0, false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotInt, gotBool := positiveIntValue(tc.value)
			if gotInt != tc.wantInt || gotBool != tc.wantBool {
				t.Fatalf("positiveIntValue(%#v) = (%d, %v), want (%d, %v)", tc.value, gotInt, gotBool, tc.wantInt, tc.wantBool)
			}
		})
	}
}

func TestEnsureReviewMap(t *testing.T) {
	t.Parallel()

	t.Run("creates map when missing", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{}
		review := ensureReviewMap(m)
		if review == nil {
			t.Fatal("ensureReviewMap(missing) = nil, want new map")
		}
		review["prNumber"] = 7
		if got, ok := m["review"].(map[string]any); !ok || got["prNumber"] != 7 {
			t.Fatalf("m[review] = %#v, want mutation reflected via stored map", m["review"])
		}
	})

	t.Run("returns existing map", func(t *testing.T) {
		t.Parallel()
		existing := map[string]any{"prUrl": "https://example.com/pr/1"}
		m := map[string]any{"review": existing}
		review := ensureReviewMap(m)
		if review["prUrl"] != "https://example.com/pr/1" {
			t.Fatalf("ensureReviewMap(existing) = %#v, want same contents as existing map", review)
		}
		review["headBranch"] = "feature"
		if existing["headBranch"] != "feature" {
			t.Fatal("ensureReviewMap(existing) did not return the same underlying map")
		}
	})

	t.Run("replaces non-map review value", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{"review": "not-a-map"}
		review := ensureReviewMap(m)
		if review == nil {
			t.Fatal("ensureReviewMap(wrong type) = nil, want new map")
		}
		if _, ok := m["review"].(map[string]any); !ok {
			t.Fatal("ensureReviewMap(wrong type) did not replace m[review] with a map")
		}
	})
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
