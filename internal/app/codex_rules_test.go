package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsCorruptCodexRulesBeforeExternalCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.Mkdir(filepath.Join(home, "rules"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "rules", "default.rules"), []byte("# policy\n\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{t: t}
	h := New(runner)
	result := h.Run(context.Background(), sampleConfig())
	if result.ExitCode != ExitPreflight || result.Err == nil || !strings.Contains(result.Err.Error(), "NUL byte") {
		t.Fatalf("expected preflight corruption failure: %+v", result)
	}
	if len(runner.calls) != 0 {
		t.Fatal("corrupt policy reached external commands")
	}
}
