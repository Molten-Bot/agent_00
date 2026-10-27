package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthDiagnosticsEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method string
		callback     bool
		err          error
		want         int
	}{
		{"available", http.MethodGet, true, nil, http.StatusOK},
		{"read only", http.MethodPost, true, nil, http.StatusMethodNotAllowed},
		{"unavailable", http.MethodGet, false, nil, http.StatusNotImplemented},
		{"sanitized failure", http.MethodGet, true, errors.New("credential-secret"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := Server{}
			calls := 0
			if tc.callback {
				s.AgentAuthDiagnostics = func(context.Context) (AgentAuthDiagnostics, error) {
					calls++
					return AgentAuthDiagnostics{Harness: "claude", Checks: []AuthDiagnosticCheck{{Code: "CLAUDE_CREDENTIALS_EMPTY", Status: "error", Description: "No access token."}}}, tc.err
				}
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, "/api/agent-auth/diagnostics", nil))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if strings.Contains(rec.Body.String(), "credential-secret") {
				t.Fatal("callback error leaked")
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("diagnostics must not be cached")
			}
			if tc.method == http.MethodPost && calls != 0 {
				t.Fatal("POST ran diagnostics")
			}
			if tc.want == http.StatusOK && !strings.Contains(rec.Body.String(), "CLAUDE_CREDENTIALS_EMPTY") {
				t.Fatal("missing machine-readable error code")
			}
		})
	}
}
