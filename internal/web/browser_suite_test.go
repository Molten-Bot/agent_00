package web

import (
	"bytes"
	"context"
	"os"
	"os/exec"
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

// browserSuiteCommand builds the node:test invocation for files. --test-force-exit
// keeps a finished suite from waiting on a browser handle Playwright left open,
// and WaitDelay keeps a killed suite from holding go test open.
func browserSuiteCommand(ctx context.Context, baseURL string, files ...string) *exec.Cmd {
	args := []string{"--test"}
	if nodeSupportsForceExit() {
		args = append(args, "--test-force-exit")
	}
	cmd := exec.CommandContext(ctx, "node", append(args, files...)...)
	cmd.Env = append(os.Environ(), "HARNESS_TEST_URL="+baseURL)
	cmd.WaitDelay = browserSuiteWaitDelay
	return cmd
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
// deadline, hiding the suite output that explains the stall.
func TestBrowserSuiteCommandStopsReadingAfterWaitDelay(t *testing.T) {
	t.Parallel()

	if browserSuiteWaitDelay <= 0 {
		t.Fatal("browser suites need a WaitDelay or a leaked child blocks go test until its own deadline")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available, skipping leaked-child check")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The backgrounded sleep keeps the inherited stdout pipe open after the
	// context kills the command itself, standing in for a leaked browser.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & exec sleep 30")
	cmd.WaitDelay = 200 * time.Millisecond

	start := time.Now()
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatal("killed suite reported success")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("killed suite blocked for %s; a leaked browser would stall go test", elapsed)
	}
}
