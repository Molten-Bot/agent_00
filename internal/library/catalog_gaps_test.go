package library

import "testing"

// TestCommitMessageTypeFallsBackOnUnrecognizedPrefix exercises the branch of
// commitMessageType where the text before ":" is not in
// conventionalCommitTypes, so the function must fall back to
// prTitleFallbackType rather than using the unrecognized word verbatim.
func TestCommitMessageTypeFallsBackOnUnrecognizedPrefix(t *testing.T) {
	t.Parallel()

	if got, want := commitMessageType("bogus: do the thing"), prTitleFallbackType; got != want {
		t.Fatalf("commitMessageType(unrecognized prefix) = %q, want %q", got, want)
	}
}

// TestDecodeTaskDefinitionKeyedPromptContextRequiredWithoutLabel exercises
// the keyed-error branch (key != "") when promptContextRequired is set but
// promptContextLabel is missing. The unkeyed branch is already covered by
// TestDecodeTaskDefinitionPromptContextValidation in
// catalog_additional_test.go.
func TestDecodeTaskDefinitionKeyedPromptContextRequiredWithoutLabel(t *testing.T) {
	t.Parallel()

	_, err := decodeTaskDefinition("tasks.json", "some-key", []byte(`{"prompt":"p","promptContextRequired":true}`))
	if err == nil {
		t.Fatal("decodeTaskDefinition(keyed, required without label) error = nil, want non-nil")
	}
	if got, want := err.Error(), `library task tasks.json key "some-key": promptContextLabel is required when promptContextRequired is true`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// TestDecodeTaskDefinitionKeyedPromptContextPlaceholderWithoutLabel
// exercises the keyed-error branch (key != "") when promptContextPlaceholder
// is set without promptContextLabel.
func TestDecodeTaskDefinitionKeyedPromptContextPlaceholderWithoutLabel(t *testing.T) {
	t.Parallel()

	_, err := decodeTaskDefinition("tasks.json", "some-key", []byte(`{"prompt":"p","promptContextPlaceholder":"e.g. a URL"}`))
	if err == nil {
		t.Fatal("decodeTaskDefinition(keyed, placeholder without label) error = nil, want non-nil")
	}
	if got, want := err.Error(), `library task tasks.json key "some-key": promptContextPlaceholder requires promptContextLabel`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
