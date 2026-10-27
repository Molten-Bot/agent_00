package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// promptEnterSuiteRoot is the module root relative to this package, which is where the
// browser suite and its Playwright install live.
const promptEnterSuiteRoot = "../.."

// promptEnterSuitePath is relative to promptEnterSuiteRoot so node resolves Playwright from
// the module root's node_modules.
const promptEnterSuitePath = "internal/web/tests/prompt_enter_submit.cjs"

// Whether Enter runs a prompt or types a newline is decided from the live user
// agent, UA-CH, and the pointer media queries, so nothing in Go can observe it.
// This wrapper boots the real UI and hands it to the Playwright suite, which
// presses the key in a headless browser; without it the suite exists but never
// runs in CI.
func TestPromptEnterSubmitBrowserSuite(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; skipping the prompt Enter browser suite")
	}
	if _, err := os.Stat(filepath.Join(promptEnterSuiteRoot, promptEnterSuitePath)); err != nil {
		t.Fatalf("browser suite is missing: %v", err)
	}
	if reason := missingPromptEnterBrowserRuntime(t, node); reason != "" {
		t.Skipf("%s; skipping the prompt Enter browser suite", reason)
	}

	srv := NewServer("", NewBroker())
	srv.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
		return ReviewSettingsState{
			MergeMethod:           "squash",
			ReviewLevel:           "off",
			DefaultLibraryEnabled: true,
			ChatEnabled:           true,
			JSONEnabled:           true,
		}, nil
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cmd := exec.Command(node, "--test", promptEnterSuitePath)
	cmd.Dir = promptEnterSuiteRoot
	cmd.Env = append(os.Environ(), "HARNESS_TEST_URL="+ts.URL)
	output, err := cmd.CombinedOutput()
	t.Logf("node --test %s output:\n%s", promptEnterSuitePath, output)
	if err != nil {
		t.Fatalf("node --test %s: %v", promptEnterSuitePath, err)
	}
}

// Playwright and its Chromium download are installed by CI rather than by the
// Go module, so a machine without them skips instead of failing. A missing
// browser is an environment gap; a real regression still fails the suite.
func missingPromptEnterBrowserRuntime(t *testing.T, node string) string {
	t.Helper()

	probe := `try {
  const fs = require('fs');
  const { chromium } = require('playwright');
  process.exit(fs.existsSync(chromium.executablePath()) ? 0 : 2);
} catch (err) {
  process.exit(3);
}`
	cmd := exec.Command(node, "-e", probe)
	cmd.Dir = promptEnterSuiteRoot
	output, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 2:
			return "playwright has no Chromium build installed"
		case 3:
			return "playwright is not installed"
		}
	}
	return "playwright could not be probed: " + err.Error() + " " + string(output)
}
