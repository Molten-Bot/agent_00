package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
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
		{"logged out CLI verdict", execx.Result{Stdout: "Not logged in · Please run /login"}, errors.New("exit status 1"), true},
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

func TestClaudeInvocationMissingConfigurationFile(t *testing.T) {
	t.Parallel()
	refusal := "Claude configuration file not found at: /home/agent/.claude/.claude.json\n" +
		"A backup file exists at: /home/agent/.claude/backups/.claude.json.backup.1793284716264\n"
	for _, tc := range []struct {
		name       string
		result     execx.Result
		err        error
		wantConfig bool
	}{
		{"stderr", execx.Result{Stderr: refusal}, errors.New("exit status 1"), true},
		{"stdout", execx.Result{Stdout: refusal}, errors.New("exit status 1"), true},
		// The CLI prints the refusal on stderr and its logged-out verdict on
		// stdout. A missing configuration file is not fixed by signing in, so
		// it must win over the authentication classification.
		{"refusal with logged out stdout", execx.Result{Stdout: "Not logged in · Please run /login", Stderr: refusal}, errors.New("exit status 1"), true},
		{"successful run", execx.Result{Stderr: refusal}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
				return tc.result, tc.err
			}))
			_, err := h.runCodexWithHeartbeat(context.Background(), agentruntime.Runtime{Harness: "claude", Command: "claude"}, t.TempDir(), "task", codexRunOptions{}, agentInvocationLogMetadata{})
			if got := errors.Is(err, agentruntime.ErrClaudeConfigMissing); got != tc.wantConfig {
				t.Fatalf("configuration failure = %v, want %v; err=%v", got, tc.wantConfig, err)
			}
			if tc.wantConfig && errors.Is(err, agentruntime.ErrClaudeAuthRequired) {
				t.Fatalf("missing configuration reported as an authentication failure: %v", err)
			}
		})
	}
}

func TestRepairClaudeConfigurationBeforeRun(t *testing.T) {
	t.Parallel()

	writeBackup := func(t *testing.T, configDir, suffix, content string) string {
		t.Helper()
		dir := filepath.Join(configDir, "backups")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, ".claude.json.backup."+suffix)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("restores from a claude backup", func(t *testing.T) {
		t.Parallel()
		configDir := t.TempDir()
		writeBackup(t, configDir, "1793284716264", `{"generation":"backup"}`)
		h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
			return execx.Result{}, nil
		}))
		if err := h.repairClaudeConfiguration([]string{"CLAUDE_CONFIG_DIR=" + configDir}); err != nil {
			t.Fatalf("repairClaudeConfiguration: %v", err)
		}
		content, err := os.ReadFile(agentruntime.ClaudeConfigPath(configDir))
		if err != nil || string(content) != `{"generation":"backup"}` {
			t.Fatalf("restored config = %q, %v", content, err)
		}
	})

	t.Run("reports an unrecoverable configuration", func(t *testing.T) {
		t.Parallel()
		configDir := t.TempDir()
		writeBackup(t, configDir, "1793284716264", "not json")
		h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
			return execx.Result{}, nil
		}))
		err := h.repairClaudeConfiguration([]string{"CLAUDE_CONFIG_DIR=" + configDir})
		if !errors.Is(err, agentruntime.ErrClaudeConfigMissing) {
			t.Fatalf("err = %v, want ErrClaudeConfigMissing", err)
		}
	})

	t.Run("an unresolvable config dir is not a task failure", func(t *testing.T) {
		t.Parallel()
		h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
			return execx.Result{}, nil
		}))
		if err := h.repairClaudeConfiguration([]string{"PATH=/usr/bin"}); err != nil {
			t.Fatalf("repairClaudeConfiguration: %v", err)
		}
	})
}

func TestRunNeedsClaudeShellTools(t *testing.T) {
	t.Parallel()

	if !runNeedsClaudeShellTools(sampleConfig()) {
		t.Fatal("runNeedsClaudeShellTools(implementation run) = false, want true")
	}

	// A review-only run gets readOnlyReviewDisallowedTools, which blocks Bash,
	// and returns before any review fix pass can run with the full tool set.
	reviewCfg := sampleConfig()
	reviewCfg.Review = &config.ReviewConfig{PRURL: "https://github.com/acme/repo/pull/1"}
	if runNeedsClaudeShellTools(reviewCfg) {
		t.Fatal("runNeedsClaudeShellTools(review-only run) = true, want false")
	}
}

func TestCheckClaudeSessionScratchFailsOnlyRunsThatNeedShellTools(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block creation here")
	}
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o700) })

	h := New(sessionRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		return execx.Result{}, nil
	}))
	environ := []string{"CLAUDE_CONFIG_DIR=" + configDir}

	// The task agent and every review fix pass would run without the tools that
	// need the scratch directory, so the run stops here instead of on review
	// convergence.
	err := h.checkClaudeSessionScratch(environ, true)
	if !errors.Is(err, agentruntime.ErrClaudeSessionScratch) {
		t.Fatalf("checkClaudeSessionScratch(needs shell tools) = %v, want ErrClaudeSessionScratch", err)
	}

	// A review-only run never reaches those tools, so the missing scratch
	// directory costs it nothing and must not fail it.
	if err := h.checkClaudeSessionScratch(environ, false); err != nil {
		t.Fatalf("checkClaudeSessionScratch(review-only run) = %v, want nil", err)
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
