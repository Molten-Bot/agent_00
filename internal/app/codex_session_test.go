package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

type sessionRunnerFunc func(context.Context, execx.Command) (execx.Result, error)

func (f sessionRunnerFunc) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	return f(ctx, cmd)
}

func TestCodexTasksIsolateRotatedCredentialsAcrossConcurrentRuns(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	authPath := filepath.Join(home, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"generation":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"configured\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "custom.md"), []byte("custom skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	h := Harness{Logf: func(string, ...any) {}, Runner: sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if active.Add(1) != 1 {
			t.Error("two Codex sessions shared a rotating login")
		}
		defer active.Add(-1)
		taskHome := envValue(cmd.Env, "CODEX_HOME")
		if taskHome == home || !strings.HasSuffix(taskHome, filepath.Join(".agent00-agent-io", "config", "codex")) {
			t.Errorf("Codex home = %s, want task-local home", taskHome)
		}
		if _, ok := environValue(cmd.Env, "AGENT00_CODEX_CREDENTIAL_SOURCE"); ok {
			t.Error("persistent credential source leaked into agent environment")
		}
		taskAuth := filepath.Join(taskHome, "auth.json")
		data, err := os.ReadFile(taskAuth)
		if err != nil {
			return execx.Result{}, err
		}
		var credential struct {
			Generation int `json:"generation"`
		}
		if err := json.Unmarshal(data, &credential); err != nil {
			return execx.Result{}, err
		}
		for path, want := range map[string]string{
			filepath.Join(taskHome, "config.toml"):         "model = \"configured\"\n",
			filepath.Join(taskHome, "skills", "custom.md"): "custom skill",
		} {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != want {
				t.Errorf("read-only Codex config %s = %q, %v; want %q", path, got, err, want)
			}
		}
		time.Sleep(time.Millisecond)
		next := filepath.Join(taskHome, ".auth-next")
		rotated := []byte(fmt.Sprintf(`{"generation":%d}`, credential.Generation+1))
		if err := os.WriteFile(next, rotated, 0o600); err != nil {
			return execx.Result{}, err
		}
		return execx.Result{Stdout: "completed"}, os.Rename(next, taskAuth)
	})}
	var wg sync.WaitGroup
	for range 12 {
		runDir := t.TempDir()
		env, err := prepareAgentIOEnv(runDir, []string{"HOME=" + home, "CODEX_HOME=" + home})
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			_, err := h.runCodexWithHeartbeat(context.Background(), agentruntime.Default(), runDir, "task", codexRunOptions{WorkspaceDir: runDir, Env: env}, agentInvocationLogMetadata{})
			if err != nil {
				t.Errorf("run: %v", err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(authPath)
	if err != nil || string(data) != `{"generation":12}` {
		t.Fatalf("persistent credentials = %q, %v; want latest rotation", data, err)
	}
}

func TestCodexAuthFailureBlocksTasksUntilFreshLogin(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	authPath := filepath.Join(home, "auth.json")
	if err := os.WriteFile(authPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	h := Harness{Logf: func(string, ...any) {}, Runner: sessionRunnerFunc(func(_ context.Context, _ execx.Command) (execx.Result, error) {
		if calls.Add(1) == 1 {
			return execx.Result{Stderr: "ERROR: Your refresh token was already used. Please log out and sign in again."}, errors.New("exit status 1")
		}
		return execx.Result{Stdout: "done"}, nil
	})}
	run := func() error {
		_, err := h.runCodexWithHeartbeat(context.Background(), agentruntime.Default(), t.TempDir(), "task", codexRunOptions{Env: []string{"CODEX_HOME=" + home}}, agentInvocationLogMetadata{})
		return err
	}
	if err := run(); !errors.Is(err, agentruntime.ErrCodexAuthRequired) {
		t.Fatalf("initial failure: %v", err)
	}
	if err := run(); !errors.Is(err, agentruntime.ErrCodexAuthRequired) {
		t.Fatalf("next task should block: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("launched another agent with rejected credentials")
	}
	if err := os.WriteFile(authPath, []byte("fresh-login"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatalf("fresh login did not recover: %v", err)
	}
}

func TestFailedCodexRunDoesNotPersistTaskCredentials(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	authPath := filepath.Join(home, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"token":"persistent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir := t.TempDir()
	env, err := prepareAgentIOEnv(runDir, []string{"HOME=" + home, "CODEX_HOME=" + home})
	if err != nil {
		t.Fatal(err)
	}
	h := Harness{Logf: func(string, ...any) {}, Runner: sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		taskAuth := filepath.Join(envValue(cmd.Env, "CODEX_HOME"), "auth.json")
		if err := os.WriteFile(taskAuth, []byte(`{"token":"untrusted"}`), 0o600); err != nil {
			return execx.Result{}, err
		}
		return execx.Result{Stderr: "transport failed"}, errors.New("exit status 1")
	})}
	if _, err := h.runCodexWithHeartbeat(context.Background(), agentruntime.Default(), runDir, "task", codexRunOptions{WorkspaceDir: runDir, Env: env}, agentInvocationLogMetadata{}); err == nil {
		t.Fatal("failed Codex run unexpectedly succeeded")
	}
	got, err := os.ReadFile(authPath)
	if err != nil || string(got) != `{"token":"persistent"}` {
		t.Fatalf("failed run changed persistent credentials: %q, %v", got, err)
	}
}

func TestCanceledCodexKeepsSessionLockUntilChildExit(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	started := make(chan struct{})
	terminated := make(chan struct{})
	h := Harness{Logf: func(string, ...any) {}, Runner: sessionRunnerFunc(func(ctx context.Context, _ execx.Command) (execx.Result, error) {
		close(started)
		<-ctx.Done()
		<-terminated // Simulate a child still being terminated by the runner.
		return execx.Result{}, ctx.Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.runCodexWithHeartbeat(ctx, agentruntime.Default(), home, "task", codexRunOptions{Env: []string{"CODEX_HOME=" + home}}, agentInvocationLogMetadata{})
		done <- err
	}()
	<-started
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer waitCancel()
	if release, err := agentruntime.AcquireCodexSession(waitCtx, home); err == nil {
		release.Release()
		t.Error("session released before child exit")
	}
	close(terminated)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	release, err := agentruntime.AcquireCodexSession(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	release.Release()
}

func TestQuotedAuthFailureDoesNotInvalidateLogin(t *testing.T) {
	t.Parallel()
	err := errors.New("exit status 1")
	quoted := execx.Result{Stderr: "user\nInvestigate refresh_token_reused and not logged in errors\nBuild failed"}
	if codexInvocationAuthenticationFailed(quoted, err) {
		t.Fatal("the echoed user prompt invalidated credentials")
	}
	diagnostic := execx.Result{Stderr: quoted.Stderr + "\n2026-10-01T14:58:13Z ERROR codex_login::auth::manager: Your refresh token was already used."}
	if !codexInvocationAuthenticationFailed(diagnostic, err) {
		t.Fatal("actual provider auth diagnostic was ignored")
	}
	if codexInvocationAuthenticationFailed(diagnostic, nil) {
		t.Fatal("successful run with a recovered auth warning invalidated credentials")
	}
}

func TestRejectedCodexLoginStopsBeforeWorkspaceAndGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := agentruntime.MarkCodexAuthenticationRequired(home); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{t: t}
	res := New(runner).Run(context.Background(), sampleConfig())
	if res.ExitCode != ExitAuth || !errors.Is(res.Err, agentruntime.ErrCodexAuthRequired) {
		t.Fatalf("rejected login = %+v", res)
	}
	if res.WorkspaceDir != "" || len(runner.calls) != 0 {
		t.Fatal("a rejected login performed workspace or git operations")
	}
}

func TestTaskSetupReusesDefaultCodexHomeWithoutReseeding(t *testing.T) {
	t.Parallel()
	operatorHome := t.TempDir()
	persistentHome := filepath.Join(operatorHome, ".codex")
	if err := os.MkdirAll(persistentHome, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(persistentHome, "auth.json")
	for _, credential := range []string{"initial-login", "rotated-login"} {
		if err := os.WriteFile(authPath, []byte(credential), 0o600); err != nil {
			t.Fatal(err)
		}
		runDir := t.TempDir()
		env, err := prepareAgentIOEnv(runDir, []string{"HOME=" + operatorHome})
		if err != nil {
			t.Fatal(err)
		}
		if got := envValue(env, "CODEX_HOME"); got != persistentHome {
			t.Fatalf("task home = %q, want persistent home %q", got, persistentHome)
		}
		if got := envValue(env, "HOME"); got == operatorHome {
			t.Fatal("task HOME isolation was lost")
		}
		if _, err := os.Stat(filepath.Join(runDir, ".agent00-agent-io", "config", "codex", "auth.json")); !os.IsNotExist(err) {
			t.Fatalf("task contains a copied credential file: %v", err)
		}
		data, err := os.ReadFile(authPath)
		if err != nil || string(data) != credential {
			t.Fatalf("task setup overwrote persistent credentials: %q, %v", data, err)
		}
	}
}

func TestWaitingForCodexLoginDoesNotConsumeExecutionTimeout(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := agentruntime.AcquireCodexSession(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Release()
	waiting := make(chan struct{})
	var once sync.Once
	h := Harness{
		AgentStageTimeout: 200 * time.Millisecond,
		Logf: func(format string, _ ...any) {
			if strings.Contains(format, "action=acquire_auth_session") {
				once.Do(func() { close(waiting) })
			}
		},
		Runner: sessionRunnerFunc(func(ctx context.Context, _ execx.Command) (execx.Result, error) {
			if err := ctx.Err(); err != nil {
				return execx.Result{}, err
			}
			return execx.Result{Stdout: "done"}, nil
		}),
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.runCodexWithHeartbeat(ctx, agentruntime.Default(), home, "task", codexRunOptions{Env: []string{"CODEX_HOME=" + home}}, agentInvocationLogMetadata{})
		done <- err
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("task did not reach the login lock")
	}
	// Deliberately wait longer than the execution timeout before allowing the
	// invocation to start. This catches a timeout applied to queueing as well.
	time.Sleep(250 * time.Millisecond)
	session.Release()
	if err := <-done; err != nil {
		t.Fatalf("login queue exhausted execution timeout: %v", err)
	}
}
