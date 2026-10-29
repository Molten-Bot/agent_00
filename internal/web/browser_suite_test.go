package web

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// browserSuiteWaitDelay bounds how long Wait keeps reading a killed suite's
// output. Headless Chromium children inherit the suite's stdout and stderr, so
// killing node on a context deadline does not close those pipes and
// CombinedOutput keeps blocking until go test hits its own deadline and panics.
// Closing the pipes turns a stalled browser suite into a reported failure.
const browserSuiteWaitDelay = 10 * time.Second

// requireBrowserTooling skips when node or the playwright module is missing so
// environments without them still run the HTTP and embedded-asset checks.
func requireBrowserTooling(t *testing.T, what string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not available, skipping %s", what)
	}
	if err := exec.Command("node", "-e", "require.resolve('playwright')").Run(); err != nil {
		t.Skipf("playwright module not available, skipping %s", what)
	}
}

// nodeSupportsForceExit reports whether the node runner accepts
// --test-force-exit, which landed in Node 22. Older toolchains run the suite
// without it.
var nodeSupportsForceExit = sync.OnceValue(func() bool {
	out, err := exec.Command("node", "--help").Output()
	return err == nil && bytes.Contains(out, []byte("--test-force-exit"))
})

// prepareBrowserSuiteCmd points cmd at baseURL and applies the WaitDelay that
// stops a killed suite's leaked children from holding go test open. Tests use
// it so they exercise the delay the suite command actually ships.
func prepareBrowserSuiteCmd(cmd *exec.Cmd, baseURL string) *exec.Cmd {
	cmd.Env = append(os.Environ(), "HARNESS_TEST_URL="+baseURL)
	cmd.WaitDelay = browserSuiteWaitDelay
	return cmd
}

// browserSuiteCommand builds the node:test invocation for files. --test-force-exit
// keeps a finished suite from waiting on a browser handle Playwright left open,
// and WaitDelay keeps a killed suite from holding go test open.
func browserSuiteCommand(ctx context.Context, baseURL string, files ...string) *exec.Cmd {
	args := []string{"--test"}
	if nodeSupportsForceExit() {
		args = append(args, "--test-force-exit")
	}
	return prepareBrowserSuiteCmd(exec.CommandContext(ctx, "node", append(args, files...)...), baseURL)
}

// runBrowserSuite runs files against baseURL and reports the suite output on
// failure, naming a timeout separately so a stalled suite stays distinguishable
// from a failing assertion.
func runBrowserSuite(t *testing.T, baseURL string, timeout time.Duration, what string, files ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := browserSuiteCommand(ctx, baseURL, files...).CombinedOutput()
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		t.Fatalf("%s timed out after %s: %v\n%s", what, timeout, ctx.Err(), out)
	}
	t.Fatalf("%s failed: %v\n%s", what, err, out)
}

func TestBrowserSuiteCommandCarriesTestURLAndWaitDelay(t *testing.T) {
	t.Parallel()

	cmd := browserSuiteCommand(context.Background(), "http://127.0.0.1:1234", "tests/example.cjs")
	if cmd.WaitDelay != browserSuiteWaitDelay {
		t.Fatalf("WaitDelay = %s, want %s; a leaked browser would hold go test open", cmd.WaitDelay, browserSuiteWaitDelay)
	}
	if !slices.Contains(cmd.Env, "HARNESS_TEST_URL=http://127.0.0.1:1234") {
		t.Fatal("suite command does not point the browser at the test server")
	}
	if got := cmd.Args[len(cmd.Args)-1]; got != "tests/example.cjs" {
		t.Fatalf("last argument = %q, want the suite file", got)
	}
	if !slices.Contains(cmd.Args, "--test") {
		t.Fatalf("args = %v, want the node test runner", cmd.Args)
	}
}

// A browser suite that stalls must fail, not hang. Chromium outlives the node
// process and holds its inherited pipes, so without WaitDelay the Go driver
// waits past its own context and only stops when go test panics on the package
// deadline, hiding the suite output that explains the stall. The shipped
// WaitDelay is asserted by TestBrowserSuiteCommandCarriesTestURLAndWaitDelay;
// spending it here would add ten idle seconds to every run, so this drives the
// same code path with a short delay of its own.
const browserSuiteProbeWaitDelay = 250 * time.Millisecond

func TestBrowserSuiteCommandStopsReadingAfterWaitDelay(t *testing.T) {
	t.Parallel()

	if browserSuiteWaitDelay <= 0 {
		t.Fatal("browser suites need a WaitDelay or a leaked child blocks go test until its own deadline")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available, skipping leaked-child check")
	}

	// The backgrounded loop stands in for a leaked browser: it inherits stdout
	// and holds it open until this file disappears, so only WaitDelay can end
	// the read. Removing the file on cleanup keeps the stand-in from outliving
	// the test the way a bare `sleep 30 &` would.
	hold := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(hold) })

	// A second is long enough for a loaded CI machine to fork the stand-in
	// before the kill, and short enough to keep this test near the WaitDelay.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	script := fmt.Sprintf("while [ -f '%s' ]; do sleep 1; done & exec sleep 300", hold)
	cmd := prepareBrowserSuiteCmd(exec.CommandContext(ctx, "sh", "-c", script), "http://127.0.0.1:1234")
	cmd.WaitDelay = browserSuiteProbeWaitDelay
	// The kill lands on the context deadline, so time the read from there: what
	// this test measures is how long Wait kept reading after the kill.
	killedAt, _ := ctx.Deadline()

	done := make(chan error, 1)
	go func() {
		_, err := cmd.CombinedOutput()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("killed suite reported success")
		}
		// A loaded machine can kill the shell before it forks the stand-in, which
		// leaves no open pipe for WaitDelay to close. That proves nothing either
		// way, so report it instead of failing on the lost race.
		if read := time.Since(killedAt); read < browserSuiteProbeWaitDelay {
			t.Skipf("stand-in child never held the pipe (read stopped %s after the kill); WaitDelay was not exercised", read)
		}
	case <-time.After(browserSuiteProbeWaitDelay + 30*time.Second):
		t.Fatalf("killed suite still reading well past its %s WaitDelay; a leaked browser would stall go test", browserSuiteProbeWaitDelay)
	}
}
