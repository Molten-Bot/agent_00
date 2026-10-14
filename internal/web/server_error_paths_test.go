package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type noFlushResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *noFlushResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *noFlushResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (w *noFlushResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
}

func decodeJSONMap(t *testing.T, body *bytes.Buffer) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.NewDecoder(bytes.NewReader(body.Bytes())).Decode(&decoded); err != nil {
		t.Fatalf("decode json body error = %v (body=%q)", err, body.String())
	}
	return decoded
}

func TestServerGitHubProfileAndIndexErrorPaths(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/github/profile", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/github/profile status = %d, want 405", resp.Code)
	}

	srv.ResolveGitHubProfileURL = func(context.Context) (string, error) {
		return "", errors.New("profile lookup failed")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/github/profile", nil)
	resp = httptest.NewRecorder()
	handler = srv.Handler()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/github/profile status = %d, want 200", resp.Code)
	}
	body := decodeJSONMap(t, resp.Body)
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("github profile error response ok = %#v, want false", body["ok"])
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "profile lookup failed") {
		t.Fatalf("github profile error = %q, want profile lookup failure", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("GET /does-not-exist status = %d, want 404", resp.Code)
	}
}

func TestServerAgentAuthStartVerifyErrorPaths(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/agent-auth/start-device", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/agent-auth/start-device status = %d, want 405", resp.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agent-auth/start-device", nil)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("POST /api/agent-auth/start-device (nil callback) status = %d, want 501", resp.Code)
	}

	srv.StartAgentAuth = func(context.Context) (AgentAuthState, error) {
		return AgentAuthState{State: "needs_device_auth"}, errors.New("auth start failed")
	}
	handler = srv.Handler()
	req = httptest.NewRequest(http.MethodPost, "/api/agent-auth/start-device", nil)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/agent-auth/start-device error status = %d, want 400", resp.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/agent-auth/verify", nil)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/agent-auth/verify status = %d, want 405", resp.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agent-auth/verify", nil)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("POST /api/agent-auth/verify (nil callback) status = %d, want 501", resp.Code)
	}

	srv.VerifyAgentAuth = func(context.Context) (AgentAuthState, error) {
		return AgentAuthState{State: "needs_device_auth"}, errors.New("auth verify failed")
	}
	handler = srv.Handler()
	req = httptest.NewRequest(http.MethodPost, "/api/agent-auth/verify", nil)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/agent-auth/verify error status = %d, want 400", resp.Code)
	}
}
