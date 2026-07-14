package failurefollowup

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendLogExcerptTransportsDecodedRedactedAgentEvidence(t *testing.T) {
	t.Parallel()

	logDir := t.TempDir()
	legacyPath := filepath.Join(logDir, LegacyTaskLogFileName)
	if err := os.WriteFile(legacyPath, []byte("legacy should not win\n"), 0o644); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
	currentPath := filepath.Join(logDir, LogFileName)
	secret := "github_" + "pat_" + strings.Repeat("x", 32)
	agentLine := "No diff created. releases.json already contains author. token=fake-secret-value bare=" + secret
	content := strings.Join([]string{
		"dispatch request_id=req-1 stage=codex status=start",
		fmt.Sprintf("dispatch request_id=req-1 cmd phase=git name=git stream=stdout b64=%s", base64.StdEncoding.EncodeToString([]byte("unrelated git output"))),
		fmt.Sprintf("dispatch request_id=req-1 cmd phase=codex name=codex stream=stdout b64=%s", base64.StdEncoding.EncodeToString([]byte(agentLine))),
		"dispatch status=no_changes request_id=req-1",
	}, "\n")
	if err := os.WriteFile(currentPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write current log: %v", err)
	}

	got := AppendLogExcerpt("Observed failure context:", []string{logDir, legacyPath, currentPath})
	for _, want := range []string{
		"Observed failure context:",
		"Prior task log excerpt",
		"No diff created. releases.json already contains author.",
		"dispatch status=no_changes request_id=req-1",
		"token=[REDACTED]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("AppendLogExcerpt() missing %q: %q", want, got)
		}
	}
	for _, notWant := range []string{secret, "unrelated git output", "legacy should not win"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("AppendLogExcerpt() contains %q: %q", notWant, got)
		}
	}
}

func TestAppendLogExcerptKeepsContextWhenLogsUnavailable(t *testing.T) {
	t.Parallel()

	if got, want := AppendLogExcerpt(" context ", []string{filepath.Join(t.TempDir(), "missing.log")}), "context"; got != want {
		t.Fatalf("AppendLogExcerpt(missing) = %q, want %q", got, want)
	}
}

func TestLogExcerptIsBoundedToTail(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), LogFileName)
	var content strings.Builder
	for i := 0; i < 100; i++ {
		text := fmt.Sprintf("agent evidence line %03d %s", i, strings.Repeat("x", 180))
		fmt.Fprintf(&content, "cmd phase=codex name=codex stream=stdout b64=%s\n", base64.StdEncoding.EncodeToString([]byte(text)))
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got := LogExcerpt([]string{path})
	if len(got) > maxFollowUpExcerptBytes+64 {
		t.Fatalf("LogExcerpt() length = %d, want bounded near %d", len(got), maxFollowUpExcerptBytes)
	}
	if !strings.Contains(got, "agent evidence line 099") {
		t.Fatalf("LogExcerpt() missing tail evidence: %q", got)
	}
	if !strings.HasPrefix(got, "...(earlier log excerpt truncated)") {
		t.Fatalf("LogExcerpt() missing truncation marker: %q", got)
	}
}
