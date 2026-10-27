package web

import "net/http"

// AgentAuthDiagnostics contains only allowlisted metadata. Never include raw
// subprocess output, credential values, OAuth URLs, codes, or account identity.
type AgentAuthDiagnostics struct {
	Harness              string                     `json:"harness"`
	State                string                     `json:"state"`
	Ready                bool                       `json:"ready"`
	CheckedAt            string                     `json:"checked_at"`
	LoginRunning         bool                       `json:"login_running"`
	LoginURLAvailable    bool                       `json:"login_url_available"`
	BrowserCodeSubmitted bool                       `json:"browser_code_submitted"`
	LastLoginExitCode    *int                       `json:"last_login_exit_code,omitempty"`
	CLI                  *AuthCLIDiagnostics        `json:"cli,omitempty"`
	Credentials          *AuthCredentialDiagnostics `json:"credentials,omitempty"`
	Checks               []AuthDiagnosticCheck      `json:"checks"`
}

type AuthCLIDiagnostics struct {
	StatusKnown bool `json:"status_known"`
	LoggedIn    bool `json:"logged_in"`
	ExitCode    *int `json:"exit_code,omitempty"`
}

type AuthCredentialDiagnostics struct {
	Present         bool   `json:"present"`
	ValidJSON       bool   `json:"valid_json"`
	HasAccessToken  bool   `json:"has_access_token"`
	HasRefreshToken bool   `json:"has_refresh_token"`
	ExpiresAt       *int64 `json:"expires_at_ms,omitempty"`
	Expired         bool   `json:"expired"`
}

type AuthDiagnosticCheck struct {
	Code        string `json:"code"`
	Status      string `json:"status"`
	Description string `json:"description"`
	NextAction  string `json:"next_action,omitempty"`
}

func (s Server) handleAgentAuthDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.AgentAuthDiagnostics == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error_code": "AUTH_DIAGNOSTICS_UNAVAILABLE", "error": "Agent authentication diagnostics are unavailable."})
		return
	}
	diagnostics, err := s.AgentAuthDiagnostics(r.Context())
	if err != nil {
		// Callback errors may contain command output or configuration values.
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error_code": "AUTH_DIAGNOSTICS_FAILED", "error": "Unable to collect authentication diagnostics."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "diagnostics": diagnostics})
}
