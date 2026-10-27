package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentReauthenticate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		method string
		err    error
		status int
	}{
		{"start", http.MethodPost, nil, http.StatusOK},
		{"failure", http.MethodPost, errors.New("login unavailable"), http.StatusBadRequest},
		{"read cannot start login", http.MethodGet, nil, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			s := Server{ReauthenticateAgent: func(context.Context) (AgentAuthState, error) {
				calls++
				return AgentAuthState{Harness: "claude", Required: true, State: "pending_browser_login"}, tc.err
			}}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest(tc.method, "/api/agent-auth/reauthenticate", nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body)
			}
			if tc.method == http.MethodPost {
				var body struct{ Auth AgentAuthState }
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Auth.Harness != "claude" || body.Auth.Ready || calls != 1 {
					t.Fatalf("response = %s, calls = %d, err = %v", response.Body, calls, err)
				}
			} else if calls != 0 {
				t.Fatal("GET started login")
			}
		})
	}
	response := httptest.NewRecorder()
	(Server{}).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/agent-auth/reauthenticate", nil))
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("unavailable status = %d", response.Code)
	}
}
