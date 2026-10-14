package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalServerHasNoRemoteConnectionSurface(t *testing.T) {
	t.Parallel()
	server := NewServer("", NewBroker())
	for _, endpoint := range []string{"/api/hub-setup", "/api/hub-setup/connect", "/api/hub-setup/disconnect"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(method, endpoint, nil))
			if response.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, endpoint, response.Code)
			}
		}
	}
	for _, name := range []string{"index.html", "site-header.js", "bottom-dock.html", "bottom-dock.js"} {
		data, err := fs.ReadFile(staticFiles, "static/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, retired := range []string{"/api/hub-setup", "hub-conn-item", "hub-setup-token", "Molten Hub", "target=hub"} {
			if strings.Contains(string(data), retired) {
				t.Errorf("%s still contains %q", name, retired)
			}
		}
	}
	data, err := json.Marshal(server.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if _, exists := snapshot["connection"]; exists {
		t.Fatal("snapshot still advertises remote connection state")
	}
}

func TestLocalReviewSettingsRemainAccessible(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	NewServer("", NewBroker()).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, expected := range []string{`id="settings-open"`, `id="settings-gate"`, `id="review-settings-form"`, `id="review-settings-save"`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("dashboard missing %s", expected)
		}
	}
}
