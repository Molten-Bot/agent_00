package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestCommandTimingPreservesResultWithoutLoggingArguments(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"git", "gh"} {
		for _, commandErr := range []error{nil, errors.New("command failed")} {
			var logs []string
			h := New(diskFullRunner{res: execx.Result{Stdout: "output"}, err: commandErr})
			h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			result, err := h.runCommandUnchecked(context.Background(), "git", execx.Command{Name: name, Args: []string{"private-argument"}, Env: []string{"PRIVATE=value"}})
			if result.Stdout != "output" || err != commandErr {
				t.Fatal("timing changed command result")
			}
			last := logs[len(logs)-1]
			outcome := "ok"
			if commandErr != nil {
				outcome = "error"
			}
			if !strings.Contains(last, "event=command_finish phase=git name="+name+" outcome="+outcome+" elapsed_ms=") || strings.Contains(last, "private-argument") || strings.Contains(last, "PRIVATE") || strings.Contains(last, "status=") {
				t.Fatalf("unsafe or missing timing: %s", last)
			}
		}
	}
}
