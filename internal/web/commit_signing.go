package web

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func (s Server) handleCommitSigning(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	verify := r.URL.Path == "/api/commit-signing/verify"
	if verify {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
	} else if !requireMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if s.CommitSigning == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "Commit signing settings are unavailable."})
		return
	}
	var state localruntime.CommitSigningState
	var err error
	switch {
	case verify:
		state, err = s.CommitSigning.Verify(r.Context())
	case r.Method == http.MethodGet:
		state, err = s.CommitSigning.Status(r.Context())
	default:
		var req localruntime.CommitSigningRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsConfigureBodyBytes))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&req)
		if decodeErr == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				decodeErr = io.ErrUnexpectedEOF
			}
		}
		if decodeErr != nil {
			// Decoder errors may quote submitted secret material. Return a fixed message.
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Invalid commit signing request."})
			return
		}
		state, err = s.CommitSigning.Configure(r.Context(), req)
	}
	// Every response carries the current hold so the browser never unblocks
	// tasks on a reply that only reports a failed setup attempt.
	state = s.CommitSigning.ApplySetupRequirement(state)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error(), "settings": state})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": state})
}
