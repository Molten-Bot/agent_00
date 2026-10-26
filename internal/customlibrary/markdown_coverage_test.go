package customlibrary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- splitFrontmatter edge cases ---------------------------------------

func TestSplitFrontmatterEdgeCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		content         string
		wantOK          bool
		wantFrontmatter string
		wantBody        string
	}{
		{
			name:     "no leading marker at all",
			content:  "Just text\nno frontmatter here.\n",
			wantOK:   false,
			wantBody: "Just text\nno frontmatter here.\n",
		},
		{
			name:     "empty content",
			content:  "",
			wantOK:   false,
			wantBody: "",
		},
		{
			name:     "opening line merely starts with the fence characters",
			content:  "---extra-stuff-on-the-same-line\nname: x\n---\nBody\n",
			wantOK:   false,
			wantBody: "---extra-stuff-on-the-same-line\nname: x\n---\nBody\n",
		},
		{
			name:     "unterminated frontmatter has no closing fence",
			content:  "---\nname: x\nBody with no closing fence\n",
			wantOK:   false,
			wantBody: "---\nname: x\nBody with no closing fence\n",
		},
		{
			name:            "closing fence may have surrounding whitespace",
			content:         "---\nname: x\n   ---   \nBody\n",
			wantOK:          true,
			wantFrontmatter: "name: x",
			wantBody:        "Body\n",
		},
		{
			name:            "CRLF line endings are normalized",
			content:         "---\r\nname: x\r\n---\r\nBody line\r\n",
			wantOK:          true,
			wantFrontmatter: "name: x",
			wantBody:        "Body line\n",
		},
		{
			name:            "leading blank lines before the opening fence are tolerated",
			content:         "\n\n---\nname: x\n---\nBody\n",
			wantOK:          true,
			wantFrontmatter: "name: x",
			wantBody:        "Body\n",
		},
		{
			name:            "empty frontmatter block",
			content:         "---\n---\nBody\n",
			wantOK:          true,
			wantFrontmatter: "",
			wantBody:        "Body\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frontmatter, body, ok := splitFrontmatter(tc.content)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && frontmatter != tc.wantFrontmatter {
				t.Fatalf("frontmatter = %q, want %q", frontmatter, tc.wantFrontmatter)
			}
			if body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// --- parseFrontmatterFields edge cases ----------------------------------

func TestParseFrontmatterFieldsCommentAndBlankLinesSkipped(t *testing.T) {
	t.Parallel()

	scalars, _, err := parseFrontmatterFields("# a leading comment\n\nname: foo\n# trailing comment\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if scalars["name"] != "foo" {
		t.Fatalf("scalars[name] = %q, want foo", scalars["name"])
	}
}

func TestParseFrontmatterFieldsLineWithoutColonRejected(t *testing.T) {
	t.Parallel()

	_, _, err := parseFrontmatterFields("name foo (no colon here)\n")
	if err == nil {
		t.Fatalf("parseFrontmatterFields() = nil error, want error for line without a colon")
	}
}

func TestParseFrontmatterFieldsEmptyKeyRejected(t *testing.T) {
	t.Parallel()

	_, _, err := parseFrontmatterFields(": value with no key\n")
	if err == nil {
		t.Fatalf("parseFrontmatterFields() = nil error, want error for empty key")
	}
}

func TestParseFrontmatterFieldsEmptyScalarWithoutFollowingBlockList(t *testing.T) {
	t.Parallel()

	// "icon:" has no value and the next line is another key (not a "- item"),
	// so it must fall back to an empty scalar rather than an array.
	scalars, arrays, err := parseFrontmatterFields("icon:\nname: foo\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if v, ok := scalars["icon"]; !ok || v != "" {
		t.Fatalf("scalars[icon] = %q, ok=%v; want empty scalar", v, ok)
	}
	if _, ok := arrays["icon"]; ok {
		t.Fatalf("arrays[icon] unexpectedly set: %v", arrays["icon"])
	}
}

func TestParseFrontmatterFieldsEmptyScalarAtEndOfFrontmatter(t *testing.T) {
	t.Parallel()

	scalars, _, err := parseFrontmatterFields("name: foo\nicon:\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if v, ok := scalars["icon"]; !ok || v != "" {
		t.Fatalf("scalars[icon] = %q, ok=%v; want empty scalar", v, ok)
	}
}

func TestParseFrontmatterFieldsDuplicateKeysLastWins(t *testing.T) {
	t.Parallel()

	scalars, _, err := parseFrontmatterFields("name: first\nname: second\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if scalars["name"] != "second" {
		t.Fatalf("scalars[name] = %q, want second (last duplicate wins)", scalars["name"])
	}
}

func TestParseFrontmatterFieldsEmptyInlineArray(t *testing.T) {
	t.Parallel()

	_, arrays, err := parseFrontmatterFields("labels: []\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if len(arrays["labels"]) != 0 {
		t.Fatalf("arrays[labels] = %v, want empty", arrays["labels"])
	}
}

func TestParseFrontmatterFieldsInlineArrayMixedQuoting(t *testing.T) {
	t.Parallel()

	_, arrays, err := parseFrontmatterFields(`labels: ["a", 'b', c]` + "\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if want := []string{"a", "b", "c"}; !equalStrings(arrays["labels"], want) {
		t.Fatalf("arrays[labels] = %v, want %v", arrays["labels"], want)
	}
}

func TestParseFrontmatterFieldsBlockListWithBlankLineBetweenItems(t *testing.T) {
	t.Parallel()

	_, arrays, err := parseFrontmatterFields("reviewers:\n  - alice\n\n  - bob\nname: foo\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if want := []string{"alice", "bob"}; !equalStrings(arrays["reviewers"], want) {
		t.Fatalf("arrays[reviewers] = %v, want %v", arrays["reviewers"], want)
	}
}

func TestParseFrontmatterFieldsBlockListZeroIndentation(t *testing.T) {
	t.Parallel()

	// Block list items need no particular indentation; only the "- " marker
	// (after trimming) matters.
	_, arrays, err := parseFrontmatterFields("reviewers:\n- alice\n- bob\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if want := []string{"alice", "bob"}; !equalStrings(arrays["reviewers"], want) {
		t.Fatalf("arrays[reviewers] = %v, want %v", arrays["reviewers"], want)
	}
}

func TestParseFrontmatterFieldsMalformedNestedInlineArray(t *testing.T) {
	t.Parallel()

	// The parser has no real nested-array support: it naively splits on top
	// level commas, so a bracket embedded in an item just rides along as
	// text. This test documents that existing (not "correct") behavior.
	_, arrays, err := parseFrontmatterFields("labels: [a, [b, c]]\n")
	if err != nil {
		t.Fatalf("parseFrontmatterFields() error: %v", err)
	}
	if want := []string{"a", "[b", "c]"}; !equalStrings(arrays["labels"], want) {
		t.Fatalf("arrays[labels] = %v, want %v", arrays["labels"], want)
	}
}

// --- parseBlockList edge cases -------------------------------------------

func TestParseBlockListEdgeCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		lines     []string
		start     int
		wantItems []string
		wantNext  int
	}{
		{
			name:      "stops immediately on a non-list line",
			lines:     []string{"name: x"},
			start:     0,
			wantItems: nil,
			wantNext:  0,
		},
		{
			name:      "blank lines within the list are skipped",
			lines:     []string{"  - alice", "", "  - bob", "next: x"},
			start:     0,
			wantItems: []string{"alice", "bob"},
			wantNext:  3,
		},
		{
			name:      "bare dash yields an empty-string item",
			lines:     []string{"  -", "  - bob"},
			start:     0,
			wantItems: []string{"", "bob"},
			wantNext:  2,
		},
		{
			name:      "quoted items are unquoted",
			lines:     []string{`  - "alice"`, "  - 'bob'", "  - charlie"},
			start:     0,
			wantItems: []string{"alice", "bob", "charlie"},
			wantNext:  3,
		},
		{
			name:      "deep indentation is tolerated",
			lines:     []string{"            - deeply indented item"},
			start:     0,
			wantItems: []string{"deeply indented item"},
			wantNext:  1,
		},
		{
			name:      "dash without a following space is not a list item",
			lines:     []string{"-nospace", "- real item"},
			start:     0,
			wantItems: nil,
			wantNext:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			items, next := parseBlockList(tc.lines, tc.start)
			if !equalStrings(items, tc.wantItems) {
				t.Fatalf("items = %v, want %v", items, tc.wantItems)
			}
			if next != tc.wantNext {
				t.Fatalf("next = %d, want %d", next, tc.wantNext)
			}
		})
	}
}

// --- parseInlineArray edge cases ------------------------------------------

func TestParseInlineArrayEdgeCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty brackets", value: "[]", want: nil},
		{name: "brackets with only whitespace", value: "[   ]", want: nil},
		{name: "single item", value: "[a]", want: []string{"a"}},
		{name: "multiple items", value: "[a, b, c]", want: []string{"a", "b", "c"}},
		{name: "mixed quoting", value: `["a", 'b', c]`, want: []string{"a", "b", "c"}},
		{name: "malformed nested brackets flatten naively", value: "[a, [b, c]]", want: []string{"a", "[b", "c]"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseInlineArray(tc.value)
			if !equalStrings(got, tc.want) {
				t.Fatalf("parseInlineArray(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// --- taskFromMarkdown edge cases ------------------------------------------

func TestTaskFromMarkdownPropagatesFrontmatterParseError(t *testing.T) {
	t.Parallel()

	content := "---\nthis line has no colon\n---\nBody.\n"
	_, err := taskFromMarkdown("acme", "broken", content)
	if err == nil {
		t.Fatalf("taskFromMarkdown() = nil error, want frontmatter parse error")
	}
}

func TestTaskFromMarkdownCRLFLineEndingsRoundTrip(t *testing.T) {
	t.Parallel()

	content := "---\r\nname: crlf-task\r\nlabels: [a, b]\r\nreviewers:\r\n  - alice\r\n  - bob\r\n---\r\nBody text here.\r\n"
	task, err := taskFromMarkdown("acme", "ignored", content)
	if err != nil {
		t.Fatalf("taskFromMarkdown() error: %v", err)
	}
	if task.Name != "acme--crlf-task" {
		t.Fatalf("Name = %q, want acme--crlf-task", task.Name)
	}
	if want := []string{"a", "b"}; !equalStrings(task.Labels, want) {
		t.Fatalf("Labels = %v, want %v", task.Labels, want)
	}
	if want := []string{"alice", "bob"}; !equalStrings(task.Reviewers, want) {
		t.Fatalf("Reviewers = %v, want %v", task.Reviewers, want)
	}
	if task.Prompt != "Body text here." {
		t.Fatalf("Prompt = %q, want %q (no stray CR)", task.Prompt, "Body text here.")
	}
	if strings.Contains(task.Prompt, "\r") {
		t.Fatalf("Prompt retained a CR: %q", task.Prompt)
	}
}

// --- loadPromptMarkdown edge cases -----------------------------------------

func TestLoadPromptMarkdownReadDirError(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := loadPromptMarkdown(missing, "acme")
	if err == nil || !strings.Contains(err.Error(), "read prompt directory") {
		t.Fatalf("loadPromptMarkdown() error = %v, want read prompt directory error", err)
	}
}

func TestLoadPromptMarkdownSkipsSubdirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a-subdir.md"), 0o700); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "task.md"), "---\nname: task\n---\nDo it.\n")

	tasks, err := loadPromptMarkdown(dir, "acme")
	if err != nil {
		t.Fatalf("loadPromptMarkdown() error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Name != "acme--task" {
		t.Fatalf("tasks = %+v, want exactly the one real file's task", tasks)
	}
}

func TestLoadPromptMarkdownReadFileError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	writeFile(t, path, "---\nname: task\n---\nDo it.\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := loadPromptMarkdown(dir, "acme")
	if err == nil || !strings.Contains(err.Error(), "read task.md") {
		t.Fatalf("loadPromptMarkdown() error = %v, want read task.md error", err)
	}
}
