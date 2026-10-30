package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
)

// prepareAgentIOEnv is what actually produces RunOptions.Env for a real run, so
// the sandbox grant has to be asserted against its output. Asserting against a
// hand-written CLAUDE_CONFIG_DIR only proves the sandbox honors the variable;
// it would keep passing if prepareAgentIOEnv stopped exporting the persistent
// directory, which is exactly the state that breaks every Bash tool call with
// "EROFS: read-only file system".
func TestPrepareAgentIOEnvGrantsPersistentClaudeConfigDirInSandbox(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	operatorHome := t.TempDir()
	configDir := filepath.Join(operatorHome, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	env, err := prepareAgentIOEnv(runDir, []string{"HOME=" + operatorHome})
	if err != nil {
		t.Fatalf("prepareAgentIOEnv() error = %v", err)
	}
	// HOME is isolated into the task workspace; the persistent Claude config
	// directory must survive that rewrite.
	if home, _ := environValue(env, "HOME"); home == operatorHome {
		t.Fatalf("HOME was not isolated into the task workspace: %q", home)
	}
	gotConfigDir, ok := environValue(env, "CLAUDE_CONFIG_DIR")
	if !ok || gotConfigDir != configDir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q (set=%t), want %q", gotConfigDir, ok, configDir)
	}

	resolvedConfigDir, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := agentruntime.Resolve(agentruntime.HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}

	for name, disallowed := range map[string][]string{
		"implementation": nil,
		"review":         readOnlyReviewDisallowedTools,
	} {
		cmd, err := rt.BuildCommand(runDir, "fix it", agentruntime.RunOptions{
			WorkspaceDir:    runDir,
			Env:             env,
			DisallowedTools: disallowed,
		})
		if err != nil {
			t.Fatalf("%s: BuildCommand() error = %v", name, err)
		}
		granted := false
		for _, arg := range cmd.Args {
			isSandboxRoots := strings.HasPrefix(arg, "sandbox_workspace_write.writable_roots=") ||
				strings.HasPrefix(arg, "permissions.agent00_review=")
			if isSandboxRoots && strings.Contains(arg, resolvedConfigDir) {
				granted = true
				break
			}
		}
		if !granted {
			t.Fatalf("%s: sandbox did not grant write access to %q: %v", name, resolvedConfigDir, cmd.Args)
		}
	}
}

// agentConfigSource returns "" when neither CLAUDE_CONFIG_DIR nor HOME is
// available, and filepath.Abs("") resolves to the harness working directory.
// Exporting that as CLAUDE_CONFIG_DIR now hands the sandbox an unrelated
// writable root, and fails the invocation outright whenever the task workspace
// sits under it, so the variable has to stay unset instead.
func TestPrepareAgentIOEnvOmitsUnresolvableClaudeConfigDir(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	env, err := prepareAgentIOEnv(runDir, []string{"CODEX_HOME=" + filepath.Join(t.TempDir(), "codex")})
	if err != nil {
		t.Fatalf("prepareAgentIOEnv() error = %v", err)
	}
	if value, ok := environValue(env, "CLAUDE_CONFIG_DIR"); ok {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want it left unset when neither it nor HOME is available", value)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := agentruntime.Resolve(agentruntime.HarnessClaude, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := rt.BuildCommand(runDir, "fix it", agentruntime.RunOptions{WorkspaceDir: runDir, Env: env})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "sandbox_workspace_write.writable_roots=") && strings.Contains(arg, cwd) {
			t.Fatalf("sandbox granted the harness working directory %q as a writable root: %q", cwd, arg)
		}
	}
}
