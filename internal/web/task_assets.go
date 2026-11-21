package web

import (
	_ "embed"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/githubassets"
)

//go:embed task_asset_auth.html
var taskAssetAuthHTML []byte

func handleTaskAssetAuth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src blob: data:; media-src blob:; frame-src blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(taskAssetAuthHTML)
}

func (s Server) handleTaskAssetPreview(w http.ResponseWriter, r *http.Request, requestID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	task, ok := s.Broker.Task(requestID)
	index, err := strconv.Atoi(r.URL.Query().Get("index"))
	if !ok || err != nil || index < 0 || index >= len(task.Assets) {
		http.NotFound(w, r)
		return
	}

	var viewerToken string
	if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		viewerToken = fields[1]
	}
	data, mediaType, err := githubassets.Preview(r.Context(), task.Assets[index], viewerToken, s.assetTransport)
	if errors.Is(err, githubassets.ErrPreviewAccess) {
		http.Error(w, "GitHub attachment requires access.", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "Preview unavailable.", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}
