package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestClaudeInvocationAuthenticationFailure(t *testing.T) {
	t.Parallel()
	diagnostic := "Failed to authenticate. API Error: 401 OAuth access token has expired. Re-authenticate to continue."
	for _, tc := range []struct {
		name     string
		result   execx.Result
		err      error
		wantAuth bool
	}{
		{"stdout", execx.Result{Stdout: diagnostic}, errors.New("exit status 1"), true},
		{"stderr", execx.Result{Stderr: diagnostic}, errors.New("exit status 1"), true},
		{"expired OAuth session", execx.Result{Stderr: "Failed to authenticate: OAuth session expired and could not be refreshed"}, errors.New("exit status 1"), true},
		{"successful recovery", execx.Result{Stdout: diagnostic}, nil, false},
		{"tool failure", execx.Result{Stderr: "curl: HTTP 401 Unauthorized"}, errors.New("exit status 1"), false},
		{"quoted prompt", execx.Result{Stdout: "Investigate: " + diagnostic}, errors.New("exit status 1"), false},
		{"ordinary failure", execx.Result{Stdout: "Build failed"}, errors.New("exit status 1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
				return tc.result, tc.err
			}))
			_, err := h.runCodexWithHeartbeat(context.Background(), agentruntime.Runtime{Harness: "claude", Command: "claude"}, t.TempDir(), "task", codexRunOptions{}, agentInvocationLogMetadata{})
			if got := errors.Is(err, agentruntime.ErrClaudeAuthRequired); got != tc.wantAuth {
				t.Fatalf("authentication required = %v, want %v; err=%v", got, tc.wantAuth, err)
			}
		})
	}
}

func TestClaudeTasksShareRefreshedCredentials(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			home := t.TempDir()
			configDir := filepath.Join(home, ".claude")
			env := []string{"HOME=" + home}
			if explicit {
				configDir = t.TempDir()
				env = append(env, "CLAUDE_CONFIG_DIR="+configDir)
			}
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			credentials := filepath.Join(configDir, ".credentials.json")
			if err := os.WriteFile(credentials, []byte("original-session"), 0o600); err != nil {
				t.Fatal(err)
			}
			first, err := prepareAgentIOEnv(t.TempDir(), env)
			if err != nil {
				t.Fatal(err)
			}
			second, err := prepareAgentIOEnv(t.TempDir(), env)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(envValue(first, "CLAUDE_CONFIG_DIR"), ".credentials.json"), []byte("refreshed-session"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{credentials, filepath.Join(envValue(second, "CLAUDE_CONFIG_DIR"), ".credentials.json")} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "refreshed-session" {
					t.Fatalf("refreshed session not shared: %q, %v", data, err)
				}
			}
		})
	}
}
