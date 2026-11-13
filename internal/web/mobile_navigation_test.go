package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestMobileNavigationInBrowser registers the mobile navigation suite with
// go test so CI exercises phone and landscape interactions in Chromium.
func TestMobileNavigationInBrowser(t *testing.T) {
	requireBrowserTooling(t, "mobile navigation checks")

	srv := NewServer("", nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	runBrowserSuite(t, ts.URL, 3*time.Minute, "mobile navigation checks", "tests/mobile_navigation.cjs")
}
