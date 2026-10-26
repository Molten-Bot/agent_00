// Package stringsx holds small string helpers shared across internal
// packages and cmd/harness, to avoid re-implementing the same logic in
// each caller.
package stringsx

import "strings"

// FirstNonEmpty returns the first value that is non-empty after trimming
// surrounding whitespace, itself trimmed. It returns "" if every value is
// empty or whitespace-only.
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
