package web

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestAgentAuthSetupInBrowser(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	if err := exec.Command("node", "-e", "require.resolve('playwright/test')").Run(); err != nil {
		t.Skip("playwright not available")
	}
	srv := NewServer("", nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "tests/agent_auth.cjs")
	cmd.Env = append(os.Environ(), "HARNESS_TEST_URL="+ts.URL)
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent auth browser checks failed: %v\n%s", err, out)
	}
}
