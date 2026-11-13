// Package jsonx holds JSON helpers shared across internal packages.
package jsonx

// StripLineComments removes JSONC-style line comments while preserving
// comment-like text inside strings. Block comments are intentionally retained.
func StripLineComments(data []byte) []byte {
	var out []byte
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		ch := data[i]

		if inString {
			out = append(out, ch)
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}

		if ch == '/' && i+1 < len(data) && data[i+1] == '/' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
			continue
		}

		out = append(out, ch)
	}

	return out
}
