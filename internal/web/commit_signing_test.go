package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestCommitSigningAPI(t *testing.T) {
	t.Parallel()
	s := NewServer("", NewBroker())
	s.CommitSigning = &localruntime.SigningSettings{Path: filepath.Join(t.TempDir(), "config.json")}
	handler := s.Handler()
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/commit-signing", "", 200},
		{"POST", "/api/commit-signing", `{"enabled":false}`, 200},
		{"POST", "/api/commit-signing", `{"private_key":"secret-key","passphrase":"secret-passphrase"}`, 400},
		{"POST", "/api/commit-signing", `{"secret-key":"value"}`, 400},
		{"POST", "/api/commit-signing", `{"enabled":false} {"secret":"data"}`, 400},
		{"DELETE", "/api/commit-signing", "", 405},
		{"GET", "/api/commit-signing/verify", "", 405},
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if rr.Code != test.status {
			t.Fatalf("%s %s: %d %s", test.method, test.path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "secret-key") || strings.Contains(rr.Body.String(), "secret-passphrase") {
			t.Fatal("secret leaked in response")
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("signing response can be cached")
		}
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/commit-signing", nil))
	var response struct {
		Settings localruntime.CommitSigningState `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Settings.Enabled || response.Settings.Configured {
		t.Fatalf("unexpected defaults: %+v", response.Settings)
	}
}

// Required signing with no saved key must reach the browser on every response,
// including the failed save attempt, so Settings can hold task submissions.
func TestCommitSigningAPIReportsSetupRequirement(t *testing.T) {
	s := NewServer("", NewBroker())
	s.CommitSigning = &localruntime.SigningSettings{
		Path:    filepath.Join(t.TempDir(), "config.json"),
		Initial: localruntime.InitConfig{CommitSigning: gitsigning.Config{Enabled: true}},
		// Keep the failed save local: an unreachable transport is a setup error too.
		GitHub: func() gitsigning.GitHub { return gitsigning.GitHub{BaseURL: "://invalid"} },
	}
	handler := s.Handler()
	for _, test := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/commit-signing", ""},
		{http.MethodPost, "/api/commit-signing", `{"enabled":true}`},
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		var response struct {
			Settings localruntime.CommitSigningState `json:"settings"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !response.Settings.SetupRequired {
			t.Fatalf("%s %s: missing setup hold: %+v", test.method, test.path, response.Settings)
		}
		if !strings.Contains(response.Settings.Message, "save a signing key first") {
			t.Fatalf("%s %s: unhelpful hold message: %q", test.method, test.path, response.Settings.Message)
		}
	}
}

// The hold crosses three layers: the Go state publishes setup_required,
// commit-signing.js turns it into the banner and the commit-signing-required
// event, and index.html listens for that event to block submissions. Renaming
// any one side silently unblocks tasks, and the browser suite that drives the
// whole flow is skipped wherever node is missing, so pin the contract here.
func TestCommitSigningHoldContractAcrossEmbeddedAssets(t *testing.T) {
	t.Parallel()
	wire, err := json.Marshal(localruntime.CommitSigningState{SetupRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"setup_required":true`) {
		t.Fatalf("signing state no longer publishes the hold the browser reads: %s", wire)
	}

	srv := NewServer("", NewBroker())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	assets := map[string]string{}
	for _, path := range []string{"/", "/chat", "/static/commit-signing.js"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %v", path, resp.StatusCode, err)
		}
		assets[path] = string(body)
	}
	for _, test := range []struct{ path, marker, why string }{
		{"/static/commit-signing.js", "settings.setup_required", "the browser stops reading the hold from the API"},
		{"/static/commit-signing.js", "'setup-required'", "the banner element is no longer filled in"},
		{"/static/commit-signing.js", "commit-signing-required", "the hold is never announced to the app"},
		{"/", `id="commit-signing-setup-required"`, "the settings banner that explains the hold is gone"},
		{"/", `window.addEventListener("commit-signing-required"`, "the app stops listening for the hold"},
		{"/", "state.commitSigningSetupRequired", "prompt submissions no longer check the hold"},
		// /chat posts to the same /api/local-prompt gate but does not load
		// index.html or commit-signing.js, so it carries its own copy of the
		// banner, the hold check, and the hand-off to the signing settings.
		{"/chat", `id="chat-commit-signing-required"`, "chat has no banner for the hold"},
		{"/chat", "settings.setup_required", "chat stops reading the hold from the API"},
		{"/chat", "signingHold.required", "chat submits prompts the server will refuse"},
		{"/chat", `href="/#settings"`, "chat cannot hand the operator to the signing settings"},
	} {
		if !strings.Contains(assets[test.path], test.marker) {
			t.Fatalf("%s no longer contains %q: %s", test.path, test.marker, test.why)
		}
	}
}

func TestCommitSigningAPIRejectsOversizedSecrets(t *testing.T) {
	t.Parallel()
	s := NewServer("", NewBroker())
	s.CommitSigning = &localruntime.SigningSettings{Path: filepath.Join(t.TempDir(), "config.json")}
	body := `{"private_key":"` + strings.Repeat("s", maxSettingsConfigureBodyBytes) + `"}`
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/commit-signing", strings.NewReader(body)))
	if rr.Code != 400 || rr.Body.Len() > 300 {
		t.Fatalf("unsafe oversized response: status=%d size=%d", rr.Code, rr.Body.Len())
	}
}
