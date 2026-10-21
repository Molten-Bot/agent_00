package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func (s Server) handleLibrarySchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.LibrarySchedules == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "library schedules are unavailable"})
		return
	}
	fail := func(status int, err error) { writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()}) }
	switch r.Method {
	case http.MethodPut:
		var input localruntime.LibraryScheduleInput
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsConfigureBodyBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			fail(http.StatusBadRequest, fmt.Errorf("invalid schedule: %w", err))
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			fail(http.StatusBadRequest, fmt.Errorf("expected one schedule object"))
			return
		}
		entry, err := s.LibrarySchedules.Save(input)
		if err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "schedule": entry})
	case http.MethodDelete:
		if err := s.LibrarySchedules.Delete(r.URL.Query().Get("key")); err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		entries, err := s.LibrarySchedules.List()
		if err != nil {
			fail(http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "schedules": entries, "maxIntervalHours": localruntime.MaxLibraryScheduleHours})
	}
}
