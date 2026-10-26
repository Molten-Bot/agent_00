package localruntime

import (
	"strings"
	"testing"
)

func TestParseRunConfigJSONRejectsEmptyPayload(t *testing.T) {
	t.Parallel()
	_, err := ParseRunConfigJSON([]byte("   "))
	if err == nil || !strings.Contains(err.Error(), "run config payload is empty") {
		t.Fatalf("ParseRunConfigJSON(blank) error = %v, want payload-empty error", err)
	}
}

func TestParseRunConfigValueMarshalError(t *testing.T) {
	t.Parallel()
	// A channel value cannot be marshaled to JSON; parseRunConfigValue is
	// called with an in-memory map (bypassing JSON decode) so this reaches
	// the marshal step directly.
	_, err := parseRunConfigValue(map[string]any{
		"repo":   "git@github.com:acme/repo.git",
		"prompt": "do work",
		"bad":    make(chan int),
	})
	if err == nil || !strings.Contains(err.Error(), "marshal run config payload") {
		t.Fatalf("parseRunConfigValue(unmarshalable) error = %v, want marshal failure", err)
	}
}

func TestParseRunConfigJSONRejectsFailedValidation(t *testing.T) {
	t.Parallel()
	_, err := ParseRunConfigJSON([]byte(`{"repo":"git@github.com:acme/repo.git","targetSubdir":"."}`))
	if err == nil || !strings.Contains(err.Error(), "validate run config payload") {
		t.Fatalf("ParseRunConfigJSON(missing prompt) error = %v, want validate failure", err)
	}
}

func TestNormalizeRunConfigMapRejectsMalformedJSONString(t *testing.T) {
	t.Parallel()
	_, err := normalizeRunConfigMap(`{"repo":`)
	if err == nil || !strings.Contains(err.Error(), "decode run config payload string") {
		t.Fatalf("normalizeRunConfigMap(malformed string) error = %v, want decode failure", err)
	}
}

func TestStringAtMissingKey(t *testing.T) {
	t.Parallel()
	if got := stringAt(map[string]any{}, "missing"); got != "" {
		t.Fatalf("stringAt(missing key) = %q, want empty", got)
	}
}

func TestStringAtNonStringValue(t *testing.T) {
	t.Parallel()
	if got := stringAt(map[string]any{"key": float64(5)}, "key"); got != "" {
		t.Fatalf("stringAt(non-string value) = %q, want empty", got)
	}
}

func TestNormalizeRunConfigAliasesPromotesTopLevelReviewFields(t *testing.T) {
	t.Parallel()

	m := map[string]any{
		"prompt":     "do work",
		"prNumber":   float64(7),
		"prUrl":      "https://example.com/pr/7",
		"headBranch": "feature/x",
	}
	if err := normalizeRunConfigAliases(m); err != nil {
		t.Fatalf("normalizeRunConfigAliases() error = %v", err)
	}
	review, ok := m["review"].(map[string]any)
	if !ok {
		t.Fatalf("m[review] = %#v, want a map created by alias promotion", m["review"])
	}
	if review["prNumber"] != 7 {
		t.Fatalf("review[prNumber] = %#v, want 7", review["prNumber"])
	}
	if review["prUrl"] != "https://example.com/pr/7" {
		t.Fatalf("review[prUrl] = %#v, want the top-level prUrl", review["prUrl"])
	}
	if review["headBranch"] != "feature/x" {
		t.Fatalf("review[headBranch] = %#v, want the top-level headBranch", review["headBranch"])
	}
}

func TestNormalizeRunConfigAliasesDoesNotOverrideExistingReviewFields(t *testing.T) {
	t.Parallel()

	m := map[string]any{
		"prompt":     "do work",
		"prNumber":   float64(7),
		"prUrl":      "https://example.com/pr/7",
		"headBranch": "feature/x",
		"review": map[string]any{
			"prNumber":   float64(99),
			"prUrl":      "https://example.com/pr/99",
			"headBranch": "feature/existing",
		},
	}
	if err := normalizeRunConfigAliases(m); err != nil {
		t.Fatalf("normalizeRunConfigAliases() error = %v", err)
	}
	review := m["review"].(map[string]any)
	if review["prNumber"] != float64(99) || review["prUrl"] != "https://example.com/pr/99" || review["headBranch"] != "feature/existing" {
		t.Fatalf("review = %#v, want existing review fields preserved", review)
	}
}

func TestCopyRunConfigAliasGuardsAgainstNilOrBlankInputs(t *testing.T) {
	t.Parallel()
	// Must not panic on a nil map or a blank canonical/alias name.
	copyRunConfigAlias(nil, "canonical", "alias")
	m := map[string]any{"alias": "value"}
	copyRunConfigAlias(m, "", "alias")
	if _, ok := m[""]; ok {
		t.Fatal("copyRunConfigAlias(blank canonical) unexpectedly wrote a key")
	}
	copyRunConfigAlias(m, "canonical", "")
	if _, ok := m["canonical"]; ok {
		t.Fatal("copyRunConfigAlias(blank alias) unexpectedly wrote a key")
	}
}

func TestNormalizeReviewRunConfigAliasesNilGuard(t *testing.T) {
	t.Parallel()
	// Must not panic.
	normalizeReviewRunConfigAliases(nil)
}

func TestNormalizeReviewRunConfigAliasesPromotesDottedAliasesWithoutExistingReviewMap(t *testing.T) {
	t.Parallel()
	m := map[string]any{"review.trigger": "comment", "review.prnumber": float64(5)}
	normalizeReviewRunConfigAliases(m)
	review, ok := m["review"].(map[string]any)
	if !ok {
		t.Fatalf("m[review] = %#v, want a map created from dotted aliases", m["review"])
	}
	if review["trigger"] != "comment" {
		t.Fatalf("review[trigger] = %#v, want comment", review["trigger"])
	}
	if review["prNumber"] != float64(5) {
		t.Fatalf("review[prNumber] = %#v, want 5", review["prNumber"])
	}
}

func TestRunConfigAliasValueSetSliceVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{"non-empty []string", []string{"a"}, true},
		{"empty []string", []string{}, false},
		{"non-empty []any", []any{"a"}, true},
		{"empty []any", []any{}, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runConfigAliasValueSet(tc.value); got != tc.want {
				t.Fatalf("runConfigAliasValueSet(%#v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestMapHasCaseInsensitiveKeyMatch(t *testing.T) {
	t.Parallel()
	m := map[string]any{"TargetSubdir": "."}
	if !mapHasCaseInsensitiveKey(m, "targetsubdir") {
		t.Fatal("mapHasCaseInsensitiveKey() = false, want true for a case-insensitive match")
	}
	if mapHasCaseInsensitiveKey(m, "other") {
		t.Fatal("mapHasCaseInsensitiveKey() = true, want false when no key matches")
	}
}

func TestIsTrustedLibraryExpansionFieldAllBranches(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"libraryTaskName", "LIBRARYTASKNAME", "libraryTaskDisplayName", "requiresNonDefaultBranch", "prompt", "PROMPT"} {
		if !isTrustedLibraryExpansionField(key) {
			t.Fatalf("isTrustedLibraryExpansionField(%q) = false, want true", key)
		}
	}
	if isTrustedLibraryExpansionField("repo") {
		t.Fatal("isTrustedLibraryExpansionField(repo) = true, want false")
	}
}

func TestNonEmptyStringArraySkipsNonStringElements(t *testing.T) {
	t.Parallel()
	got := nonEmptyStringArray([]any{"a", 5, "", "b", nil})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("nonEmptyStringArray() = %#v, want [a b]", got)
	}
}
