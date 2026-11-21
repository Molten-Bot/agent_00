package web

import (
	_ "embed"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
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
	assetURL, err := url.PathUnescape(r.Header.Get("X-Task-Asset-URL"))
	if !ok || err != nil || assetURL == "" {
		http.NotFound(w, r)
		return
	}
	index := slices.IndexFunc(task.Assets, func(asset githubassets.Asset) bool { return asset.URL == assetURL })
	if index == -1 {
		http.NotFound(w, r)
		return
	}

	var viewerToken string
	if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		viewerToken = fields[1]
	}
	body, mediaType, err := githubassets.Preview(r.Context(), task.Assets[index], viewerToken, s.assetTransport)
	if errors.Is(err, githubassets.ErrPreviewAccess) {
		http.Error(w, "GitHub attachment requires access.", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "Preview unavailable.", http.StatusBadGateway)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var dst io.Writer = w
	if flusher, ok := w.(http.Flusher); ok && strings.HasPrefix(mediaType, "video/") {
		dst = taskVideoWriter{ResponseWriter: w, Flusher: flusher}
	}
	if _, err := io.Copy(dst, body); err != nil {
		// Headers may already be sent. Abort so an incomplete preview fails the fetch.
		panic(http.ErrAbortHandler)
	}
}

type taskVideoWriter struct {
	http.ResponseWriter
	http.Flusher
}

func (w taskVideoWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		w.Flush()
	}
	return n, err
}
