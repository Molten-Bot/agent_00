package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Molten-Bot/agent_00/internal/customlibrary"
)

func (s Server) handleCustomLibrarySources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.CustomLibrarySources == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "custom library sources are unavailable"})
		return
	}
	fail := func(status int, err error) { writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()}) }

	switch r.Method {
	case http.MethodPost:
		var input customlibrary.Source
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsConfigureBodyBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			fail(http.StatusBadRequest, fmt.Errorf("invalid library source: %w", err))
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			fail(http.StatusBadRequest, fmt.Errorf("expected one library source object"))
			return
		}
		entry, err := s.CustomLibrarySources.Add(r.Context(), input)
		if err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "source": entry})
	case http.MethodDelete:
		if err := s.CustomLibrarySources.Remove(r.URL.Query().Get("name")); err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		entries, err := s.CustomLibrarySources.List()
		if err != nil {
			fail(http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sources": entries})
	}
}
