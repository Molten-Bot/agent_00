package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// This opt-in smoke test invokes the real provider and uses its persistent login.
// It exercises task IO isolation, session locking and the generated agent command
// without cloning, pushing, creating a PR or starting the Hub daemon.
func TestCodexTaskStartup(t *testing.T) {
	if os.Getenv("HARNESS_TEST_CODEX_TASK") != "1" {
		t.Skip("set HARNESS_TEST_CODEX_TASK=1 with an authenticated Codex CLI to run a real task")
	}
	// TestMain isolates CODEX_HOME for unit tests. Use a separate, explicit opt-in
	// path so this test alone can use the operator's real login.
	loginHome := os.Getenv("HARNESS_TEST_CODEX_HOME")
	if loginHome == "" {
		t.Fatal("HARNESS_TEST_CODEX_HOME must point to the persistent authenticated Codex home")
	}
	t.Setenv("CODEX_HOME", loginHome)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("agent-startup-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := prepareAgentIOEnv(root, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	home, err := agentruntime.CodexHome(env)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session, err := agentruntime.AcquireCodexSession(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Release()
	if err := agentruntime.CheckCodexAuthentication(home); err != nil {
		t.Fatal(err)
	}
	cmd, err := agentCommandWithOptions(agentruntime.Default(),
		repo,
		"Local startup smoke test. Use the shell tool to run `pwd` and `cat input.txt > startup-smoke.txt`, then use the shell to verify the output. Do not inspect credentials, use network tools, commit, push or create a PR. Finish with agent-startup-ok.",
		codexRunOptions{WorkspaceDir: root, SkipGitRepoCheck: true, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	cmd.InheritedFiles = session.ChildFiles()
	cmd.Args = append(cmd.Args, "--json")
	result, err := (execx.OSRunner{}).Run(ctx, cmd)
	if err != nil {
		if agentruntime.CodexAuthenticationFailure(result.Stdout + "\n" + result.Stderr) {
			if markErr := agentruntime.MarkCodexAuthenticationRequired(home); markErr != nil {
				t.Logf("record authentication failure: %v", markErr)
			}
		}
		t.Fatalf("agent startup failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "startup-smoke.txt"))
	if err != nil {
		t.Fatalf("agent did not complete the shell task: %v\n%s", err, result.Stderr)
	}
	if string(data) != "agent-startup-ok\n" {
		t.Fatalf("unexpected smoke artifact: %q", data)
	}
	var shellCompleted, agentConfirmed bool
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ExitCode *int   `json:"exit_code"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid Codex JSON event: %v", err)
		}
		if event.Type != "item.completed" {
			continue
		}
		if event.Item.Type == "command_execution" && event.Item.ExitCode != nil && *event.Item.ExitCode == 0 {
			shellCompleted = true
		}
		if event.Item.Type == "agent_message" && strings.Contains(event.Item.Text, "agent-startup-ok") {
			agentConfirmed = true
		}
	}
	if !shellCompleted || !agentConfirmed {
		t.Fatalf("incomplete task: successful shell command=%t, agent confirmation=%t", shellCompleted, agentConfirmed)
	}
	t.Log("authenticated Codex task executed shell commands and produced the verified artifact")
}
