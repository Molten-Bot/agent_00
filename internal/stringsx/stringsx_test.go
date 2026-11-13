package stringsx

import "testing"

func TestContainsAny(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		text    string
		markers []string
		want    bool
	}{
		{name: "nil markers", text: "some text"},
		{name: "no match", text: "some text", markers: []string{"xyz", "abc"}},
		{name: "match first", text: "hello world", markers: []string{"hello", "xyz"}, want: true},
		{name: "match last", text: "hello world", markers: []string{"xyz", "world"}, want: true},
		{name: "empty marker", text: "anything", markers: []string{""}, want: true},
		{name: "case sensitive", text: "Hello", markers: []string{"hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ContainsAny(tt.text, tt.markers); got != tt.want {
				t.Fatalf("ContainsAny(%q, %v) = %v, want %v", tt.text, tt.markers, got, tt.want)
			}
		})
	}
}

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
