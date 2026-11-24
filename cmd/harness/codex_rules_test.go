package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestBootDiagnosticsRejectCorruptCodexRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.Mkdir(filepath.Join(home, "rules"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "rules", "default.rules"), []byte("# policy\n\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	runner := repairPRRunner(func(context.Context, execx.Command) (execx.Result, error) {
		return execx.Result{Stdout: "available"}, nil
	})
	runBootDiagnostics(context.Background(), runner, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}, localruntime.InitConfig{AgentHarness: "codex"})
	output := strings.Join(logs, "\n")
	if !strings.Contains(output, "status=error requirement=codex_policy_files") || !strings.Contains(output, "required_checks=failed count=1") || strings.Contains(output, "required_checks=ok") {
		t.Fatalf("corrupt policy reported ready: %s", output)
	}
}
