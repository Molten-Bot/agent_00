package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
