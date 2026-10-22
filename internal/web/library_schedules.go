package web

import (
	"net/http"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func (s Server) handleLibrarySchedules(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPut, http.MethodDelete) {
		return
	}
	if s.LibrarySchedules == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "library schedules are unavailable"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		input, err := decodeStrictJSON[localruntime.LibraryScheduleInput](w, r, maxSettingsConfigureBodyBytes, "invalid schedule", "expected one schedule object")
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		entry, err := s.LibrarySchedules.Save(input)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "schedule": entry})
	case http.MethodDelete:
		if err := s.LibrarySchedules.Delete(r.URL.Query().Get("key")); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		entries, err := s.LibrarySchedules.List()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "schedules": entries, "maxIntervalHours": localruntime.MaxLibraryScheduleHours})
	}
}
