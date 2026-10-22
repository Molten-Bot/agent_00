package web

import (
	"net/http"

	"github.com/Molten-Bot/agent_00/internal/customlibrary"
)

func (s Server) handleCustomLibrarySources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet, http.MethodPost, http.MethodDelete) {
		return
	}
	if s.CustomLibrarySources == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "custom library sources are unavailable"})
		return
	}

	switch r.Method {
	case http.MethodPost:
		input, err := decodeStrictJSON[customlibrary.Source](w, r, maxSettingsConfigureBodyBytes, "invalid library source", "expected one library source object")
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		entry, err := s.CustomLibrarySources.Add(r.Context(), input)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "source": entry})
	case http.MethodDelete:
		if err := s.CustomLibrarySources.Remove(r.URL.Query().Get("name")); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		entries, err := s.CustomLibrarySources.List()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sources": entries})
	}
}
