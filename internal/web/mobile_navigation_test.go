package web

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMobileNavigationInBrowser registers the mobile navigation suite with
// go test so CI exercises phone and landscape interactions in Chromium.
func TestMobileNavigationInBrowser(t *testing.T) {
	requireBrowserTooling(t, "mobile navigation checks")

	broker := NewBroker()
	broker.IngestLog("task status=start request_id=terminal-short")
	broker.IngestLog("task status=start request_id=terminal-long")
	for i := 1; i <= 1800; i++ {
		broker.IngestLog(fmt.Sprintf("task request_id=terminal-long cmd phase=codex name=codex stream=stdout text=%q", fmt.Sprintf("Output line %d", i)))
	}
	srv := NewServer("", broker)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	runBrowserSuite(t, ts.URL, 3*time.Minute, "mobile navigation checks", "tests/mobile_navigation.cjs")
}
