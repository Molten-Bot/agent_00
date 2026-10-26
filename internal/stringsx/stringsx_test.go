package stringsx

import "testing"

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   string
	}{
		{"all empty", []string{"", "", ""}, ""},
		{"no args", nil, ""},
		{"whitespace only", []string{"  ", "\t"}, ""},
		{"first wins", []string{"a", "b"}, "a"},
		{"skips leading empties", []string{"", "  ", "b", "c"}, "b"},
		{"trims result", []string{"  b  "}, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstNonEmpty(tc.values...); got != tc.want {
				t.Fatalf("FirstNonEmpty(%#v) = %q, want %q", tc.values, got, tc.want)
			}
		})
	}
}
