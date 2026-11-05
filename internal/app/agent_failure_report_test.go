package app

import (
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

// markdownFailureReport mirrors the shape agents actually produce for the
// mandated Failure/Error details fields: a Markdown report that leads with its
// summary, so both fields sit well above the terminal output window.
func markdownFailureReport() string {
	return strings.Join([]string{
		"## Failure",
		"",
		"**Failure:** Cannot complete the requested change. Zero repository changes made.",
		"",
		"**Error details:**",
		"",
		"```",
		"EROFS: read-only file system, mkdir '/config/home/.claude/session-env'",
		"```",
		"",
		"Bash fails during session bootstrap, before any command runs.",
		"",
		"### Unblock path",
		"",
		"- make the configuration directory writable, or",
		"- point the configuration directory at a writable path.",
		"",
		"Nothing was changed.",
	}, "\n")
}

func TestCodexReportedFailureDetectsMarkdownFailureFields(t *testing.T) {
	t.Parallel()

	res := execx.Result{Stdout: markdownFailureReport()}
	failed, detail := codexReportedFailure(res)
	if !failed {
		t.Fatal("codexReportedFailure() = false, want true for a Markdown failure report")
	}
	if !strings.HasPrefix(detail, "Failure: Cannot complete the requested change.") {
		t.Fatalf("detail = %q, want the reported failure summary", detail)
	}

	full := codexFailureDetailWithErrorDetails(res, detail)
	if !strings.Contains(full, "read-only file system") {
		t.Fatalf("detail = %q, want the reported error details", full)
	}
	if strings.Contains(full, "```") {
		t.Fatalf("detail = %q, want Markdown code fences dropped", full)
	}
}

func TestCodexReportedFailureIgnoresFailureFieldWithoutErrorDetails(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"**Failure:** one validation command could not run.",
		"Retried and recovered.",
		"step 3",
		"step 4",
		"step 5",
		"step 6",
		"step 7",
		"step 8",
		"step 9",
		"step 10",
		"Done.",
	}, "\n")
	if failed, detail := codexReportedFailure(execx.Result{Stdout: output}); failed {
		t.Fatalf("codexReportedFailure() = true (%q), want false without reported error details", detail)
	}
}

func TestAgentFailureFieldLineStripsMarkdownDecoration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		line string
		want string
	}{
		{"**Failure:** broke", "Failure: broke"},
		{"- **Error details:**", "Error details:"},
		{"> *Failure:* broke", "Failure: broke"},
		{"## Failure", "Failure"},
		{"Failure: broke", "Failure: broke"},
		{`"summary": "Task failed"`, `"summary": "Task failed"`},
	}
	for _, tc := range cases {
		if got := agentFailureFieldLine(tc.line); got != tc.want {
			t.Fatalf("agentFailureFieldLine(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}
