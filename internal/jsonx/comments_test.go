package jsonx

import "testing"

func TestStripLineComments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "trailing comment", data: "{\"a\":1} // trailing comment\n\"b\"\n", want: "{\"a\":1} \n\"b\"\n"},
		{name: "comment at EOF", data: `{"a":1} // comment`, want: `{"a":1} `},
		{name: "slashes in string", data: `{"url":"https://example.com"}`, want: `{"url":"https://example.com"}`},
		{name: "escaped backslash before quote", data: `{"a":"x\\"} // comment`, want: `{"a":"x\\"} `},
		{name: "escaped character", data: `{"a":"line\n// not a comment"}`, want: `{"a":"line\n// not a comment"}`},
		{name: "block comment retained", data: `{"a":1} /* not stripped */`, want: `{"a":1} /* not stripped */`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(StripLineComments([]byte(tt.data))); got != tt.want {
				t.Fatalf("StripLineComments() = %q, want %q", got, tt.want)
			}
		})
	}
}
