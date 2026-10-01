package agentruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestCodexHome(t *testing.T) {
	t.Parallel()
	operatorHome := t.TempDir()
	persistentHome := t.TempDir()
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{[]string{"HOME=" + operatorHome}, filepath.Join(operatorHome, ".codex")},
		{[]string{"HOME=" + operatorHome, "CODEX_HOME=/old", "CODEX_HOME=" + persistentHome}, persistentHome},
		{[]string{"HOME=" + operatorHome, "CODEX_HOME= "}, filepath.Join(operatorHome, ".codex")},
	} {
		got, err := CodexHome(tc.env)
		if err != nil || got != tc.want {
			t.Fatalf("CodexHome = %q, %v; want %q", got, err, tc.want)
		}
	}
	if _, err := CodexHome([]string{"PATH=/bin"}); err == nil {
		t.Fatal("missing home must fail")
	}
}

func TestCodexSessionCancellationAndIndependentHomes(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	release, err := AcquireCodexSession(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer release.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := AcquireCodexSession(ctx, home); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquisition = %v", err)
	}
	otherRelease, err := AcquireCodexSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherRelease.Release()
}

func TestCodexSessionSubprocess(t *testing.T) {
	if home := os.Getenv("AGENT00_LOCK_TEST_HOME"); home != "" {
		if os.Getenv("AGENT00_LOCK_TEST_INHERITED") == "" {
			release, err := AcquireCodexSession(context.Background(), home)
			if err != nil {
				t.Fatal(err)
			}
			defer release.Release()
		}
		fmt.Fprintln(os.Stdout, "locked")
		if signalFile := os.Getenv("AGENT00_LOCK_TEST_SIGNAL_FILE"); signalFile != "" {
			for {
				if _, err := os.Stat(signalFile); err == nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		var data [1]byte
		_, _ = os.Stdin.Read(data[:])
		return
	}
	t.Parallel()
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCodexSessionSubprocess$")
	cmd.Env = append(os.Environ(), "AGENT00_LOCK_TEST_HOME="+home)
	var parentSession *CodexSession
	if runtime.GOOS != "windows" {
		var err error
		parentSession, err = AcquireCodexSession(context.Background(), home)
		if err != nil {
			t.Fatal(err)
		}
		defer parentSession.Release()
		cmd.ExtraFiles = parentSession.ChildFiles()
		cmd.Env = append(cmd.Env, "AGENT00_LOCK_TEST_INHERITED=1")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "locked" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child did not acquire lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child startup timed out")
	}
	if parentSession != nil {
		// Simulate the harness exiting. The inherited descriptor must protect
		// credentials until its still-running child exits too.
		parentSession.Release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := AcquireCodexSession(ctx, home); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process acquisition = %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := AcquireCodexSession(ctx, home)
	if err != nil {
		t.Fatalf("crashed child retained lock: %v", err)
	}
	release.Release()
}

func TestCodexSessionInheritedThroughSubprocessRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not inherit Unix lock file descriptors")
	}
	t.Parallel()
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := AcquireCodexSession(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Release()
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := (execx.OSRunner{}).RunStream(ctx, execx.Command{
			Name: os.Args[0], Args: []string{"-test.run=^TestCodexSessionSubprocess$"},
			Env: append(os.Environ(), "AGENT00_LOCK_TEST_HOME="+home, "AGENT00_LOCK_TEST_INHERITED=1",
				"AGENT00_LOCK_TEST_SIGNAL_FILE="+filepath.Join(home, "finish")),
			InheritedFiles: session.ChildFiles(),
		}, func(stream, line string) {
			if stream == "stdout" && line == "locked" {
				started <- struct{}{}
			}
		})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("subprocess runner did not start the child")
	}
	session.Release()
	waitCtx, waitCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer waitCancel()
	if nextSession, err := AcquireCodexSession(waitCtx, home); err == nil {
		nextSession.Release()
		t.Error("subprocess runner failed to pass the session lock to its child")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting for inherited session: %v", err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled subprocess unexpectedly completed")
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), time.Second)
	defer recoveryCancel()
	nextSession, err := AcquireCodexSession(recoveryCtx, home)
	if err != nil {
		t.Fatalf("terminated child retained its session lock: %v", err)
	}
	nextSession.Release()
}

func TestCodexAuthenticationFailureRecovery(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	authPath := filepath.Join(home, "auth.json")
	old := `{"tokens":{"refresh_token":"fake-old-token"}}`
	if err := os.WriteFile(authPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MarkCodexAuthenticationRequired(home); err != nil {
		t.Fatal(err)
	}
	if err := CheckCodexAuthentication(home); !errors.Is(err, ErrCodexAuthRequired) {
		t.Fatalf("failed generation allowed: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(home, codexAuthFailureFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(marker), "fake-old-token") {
		t.Fatal("marker contains credentials")
	}
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"refresh_token":"fake-new-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckCodexAuthentication(home); err != nil {
		t.Fatalf("fresh login blocked: %v", err)
	}
	if err := MarkCodexAuthenticationRequired(home); err != nil {
		t.Fatal(err)
	}
	if err := ClearCodexAuthenticationRequired(home); err != nil {
		t.Fatal(err)
	}
	if err := CheckCodexAuthentication(home); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAuthenticationFailureClassification(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"refresh_token_reused", "Your refresh token has already been used to generate a new access token.",
		"Your access token could not be refreshed because your refresh token was already used.",
		"refresh_token_expired", "refresh_token_invalidated", "Could not parse your authentication token.",
	} {
		if !CodexAuthenticationFailure(message) {
			t.Errorf("unrecognized auth failure: %s", message)
		}
	}
	for _, message := range []string{"MCP server: HTTP 401 Unauthorized", "bubblewrap not found", "test failed"} {
		if CodexAuthenticationFailure(message) {
			t.Errorf("misclassified failure: %s", message)
		}
	}
}
