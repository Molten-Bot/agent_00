package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src blob: data:; media-src 'self' blob:; frame-src blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
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
	videoID := r.URL.Query().Get("video")
	if !ok || err != nil || (assetURL == "") == (videoID == "") {
		http.NotFound(w, r)
		return
	}
	index := slices.IndexFunc(task.Assets, func(asset githubassets.Asset) bool {
		if videoID != "" {
			return taskAssetVideoID(asset.URL) == videoID
		}
		return asset.URL == assetURL
	})
	if index == -1 {
		http.NotFound(w, r)
		return
	}

	var viewerToken string
	if fields := strings.Fields(r.Header.Get("Authorization")); videoID == "" && len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		viewerToken = fields[1]
	}
	if playback := r.URL.Query().Get("playback"); playback != "" {
		grant, err := s.openAssetPlayback(playback)
		if err != nil || videoID == "" || grant.Request != requestID || grant.Video != videoID || time.Now().Unix() >= grant.Expires {
			http.NotFound(w, r)
			return
		}
		viewerToken = grant.Token
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
	defer func() { body.Close() }()

	isVideo := strings.HasPrefix(mediaType, "video/")
	if videoID != "" && !isVideo {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isVideo && videoID == "" && r.Header.Get("X-Task-Asset-Playback") == "native" {
		// Let the media element own the stream. Use the attachment identity
		// without putting the upstream URL in the playback request.
		w.Header().Set("X-Task-Asset-Video", taskAssetVideoID(assetURL))
		if viewerToken != "" {
			grant := assetPlaybackGrant{requestID, taskAssetVideoID(assetURL), viewerToken, time.Now().Add(30 * time.Minute).Unix()}
			data, _ := json.Marshal(grant)
			aead, err := s.assetPlaybackCipher()
			if err != nil {
				http.Error(w, "Preview unavailable.", http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Task-Asset-Playback", base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nil, data, nil)))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	var dst io.Writer = w
	if flusher, ok := w.(http.Flusher); ok && isVideo {
		dst = taskVideoWriter{ResponseWriter: w, Flusher: flusher}
	}
	if video, ok := body.(*githubassets.VideoPreviewBody); ok && video.ContentLength() > 0 {
		size := video.ContentLength()
		w.Header().Set("Accept-Ranges", "bytes")
		// Without validators, If-Range always falls back to the full response.
		if spec, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok && !strings.Contains(spec, ",") && r.Header.Get("If-Range") == "" {
			start, end, ok := taskVideoByteRange(spec, size)
			if !ok {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
				http.Error(w, "Invalid byte range.", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if start > 0 {
				// Preview has sniffed the format. Fetch the requested bytes
				// directly, retaining the original stream for upstreams without ranges.
				ranged, err := video.OpenRange(r.Context(), start, end)
				if err != nil {
					http.Error(w, "Preview unavailable.", http.StatusBadGateway)
					return
				}
				if ranged != nil {
					body.Close()
					body = ranged
				} else if _, err := io.CopyN(io.Discard, body, start); err != nil {
					panic(http.ErrAbortHandler)
				}
			}
			length := end - start + 1
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
			w.WriteHeader(http.StatusPartialContent)
			if _, err := io.CopyN(dst, body, length); err != nil {
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
	if _, err := io.Copy(dst, body); err != nil {
		// Abort so the client sees a failed stream even if headers were already sent.
		panic(http.ErrAbortHandler)
	}
}

func taskVideoByteRange(spec string, size int64) (start, end int64, ok bool) {
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false
	}
	end = size - 1
	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		return size - min(suffix, size), end, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if last != "" {
		lastByte, err := strconv.ParseInt(last, 10, 64)
		if err != nil || lastByte < start {
			return 0, 0, false
		}
		end = min(lastByte, end)
	}
	return start, end, true
}

func taskAssetVideoID(assetURL string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(assetURL)))
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

// Playback capabilities expose only this attachment, never the viewer's GitHub
// token. They expire after 30 minutes and become invalid on server restart.
type assetPlaybackGrant struct {
	Request string
	Video   string
	Token   string
	Expires int64
}

func (s Server) assetPlaybackCipher() (cipher.AEAD, error) {
	if len(s.assetPlaybackKey) == 0 {
		return nil, errors.New("playback key unavailable")
	}
	key := sha256.Sum256(s.assetPlaybackKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (s Server) openAssetPlayback(value string) (assetPlaybackGrant, error) {
	var grant assetPlaybackGrant
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return grant, err
	}
	aead, err := s.assetPlaybackCipher()
	if err != nil {
		return grant, err
	}
	data, err = aead.Open(nil, nil, data, nil)
	if err != nil {
		return grant, err
	}
	err = json.Unmarshal(data, &grant)
	return grant, err
}
