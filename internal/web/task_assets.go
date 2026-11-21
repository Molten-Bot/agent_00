package web

import (
	"net/http"
	"strconv"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubassets"
)

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

	runner := s.GitHubIssueRunner
	if runner == nil {
		runner = execx.OSRunner{}
	}
	token := githubassets.AuthToken(r.Context(), runner)
	data, mediaType, err := githubassets.Preview(r.Context(), task.Assets[index], token, s.assetTransport)
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
