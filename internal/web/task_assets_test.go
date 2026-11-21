package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

type taskAssetTransportFunc func(*http.Request) (*http.Response, error)

//go:embed testdata/task-asset-preview.webm
var taskAssetTestVideo []byte // Four seconds of VP8 in a live WebM container.

func (f taskAssetTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestTaskAssetsRenderInBrowser(t *testing.T) {
	requireBrowserTooling(t, "task asset checks")
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("preview looked up server GitHub credentials")
		return execx.Result{}, fmt.Errorf("unexpected GitHub CLI command")
	}), true)
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "github.com" || req.Header.Get("Cookie") != "" {
			return nil, fmt.Errorf("unexpected preview host or cookie")
		}
		status, mediaType, body := http.StatusNotFound, "text/plain", ""
		headers := make(http.Header)
		if req.Header.Get("Authorization") != "" && req.URL.Path != "/user-attachments/assets/unavailable" {
			t.Error("viewer credentials were reused for another attachment")
		}
		switch req.URL.Path {
		case "/user-attachments/assets/screenshot-id":
			status, mediaType, body = http.StatusOK, "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360"><rect width="640" height="360" fill="#246"/></svg>`
		case "/user-attachments/files/42/spec.pdf":
			status, mediaType, body = http.StatusOK, "application/octet-stream", taskAssetTestPDF()
		case "/user-attachments/files/42/notes.txt":
			status, mediaType, body = http.StatusOK, "text/plain", "<script>throw new Error('attachment executed')</script>\nUTF-8: café"
		case "/user-attachments/files/42/page.html":
			status, mediaType, body = http.StatusOK, "text/html", "<!DOCTYPE html>\n<strong>Café</strong>"
			headers.Set("Content-Disposition", `attachment; filename="page.html"`)
		case "/user-attachments/files/42/tone.wav":
			status, mediaType, body = http.StatusOK, "application/octet-stream", taskAssetTestWAV()
		case "/user-attachments/files/42/silence.mp3":
			// Untagged MPEG-1 Layer III silence, 128 kbps at 44.1 kHz.
			frame := "\xff\xfb\x90\x64" + strings.Repeat("\x00", 413)
			status, mediaType, body = http.StatusOK, "application/octet-stream", strings.Repeat(frame, 8)
		case "/user-attachments/assets/video":
			// Keep the upstream open so playback must start before EOF.
			tail, finish := io.Pipe()
			context.AfterFunc(req.Context(), func() { finish.Close() })
			return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(taskAssetTestVideo), tail), tail}}, nil
		case "/user-attachments/assets/unavailable":
			if req.Header.Get("Authorization") == "Bearer viewer-fixture" {
				status, body = http.StatusOK, "Private attachment preview"
			}
		case "/user-attachments/assets/unsupported":
			status, body = http.StatusOK, "\x00\xff"
		}
		headers.Set("Content-Type", mediaType)
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	run := issueRunConfig(GitHubRepo{FullName: "example/project"}, GitHubIssue{
		Number: 17, Title: "Inspect the attachments", State: "OPEN",
		Body:     "![Screenshot](https://github.com/user-attachments/assets/screenshot-id)",
		Comments: []GitHubIssueComment{{Author: GitHubIssueUser{Login: "reviewer"}, Body: "[Specification](https://github.com/user-attachments/files/42/spec.pdf)\n![Duplicate screenshot](https://github.com/user-attachments/assets/screenshot-id)"}},
	}, config.GitHubIssueLinkReference)
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	srv.Broker.RecordTaskRunConfig("req-assets", raw)
	srv.Broker.RecordTaskRunConfig("req-single-asset", []byte(`{"repo":"example/project","prompt":"Task with one attachment","assets":[{"url":"https://github.com/user-attachments/assets/unavailable","name":"unavailable"}]}`))
	srv.Broker.RecordTaskRunConfig("req-unsupported-asset", []byte(`{"repo":"example/project","prompt":"Task with an unsupported attachment","assets":[{"url":"https://github.com/user-attachments/assets/unsupported","name":"unsupported"}]}`))
	srv.Broker.RecordTaskRunConfig("req-text-asset", []byte(`{"repo":"example/project","prompt":"Task with text attachments","assets":[
		{"url":"https://github.com/user-attachments/files/42/notes.txt","name":"notes.txt"},
		{"url":"https://github.com/user-attachments/files/42/page.html","name":"page.html"}
	]}`))
	srv.Broker.RecordTaskRunConfig("req-audio-asset", []byte(`{"repo":"example/project","prompt":"Task with audio attachments","assets":[{"url":"https://github.com/user-attachments/files/42/tone.wav","name":"tone.wav"},{"url":"https://github.com/user-attachments/files/42/silence.mp3","name":"silence.mp3"}]}`))
	srv.Broker.RecordTaskRunConfig("req-video-asset", []byte(`{"repo":"example/project","prompt":"Task with video","assets":[{"url":"https://github.com/user-attachments/assets/video","name":"Recording"}]}`))
	srv.Broker.RecordTaskRunConfig("req-private-images", []byte(`{"repo":"example/project","prompt":"Task with signed images","assets":[
		{"url":"https://private-user-images.githubusercontent.com/42/private.png?jwt=fixture%2Bsignature","name":"Private screenshot"},
		{"url":"https://private-user-images.githubusercontent.com/42/expired.png?jwt=expired-fixture","name":"Expired screenshot"}
	]}`))
	srv.Broker.RecordTaskRunConfig("req-no-assets", []byte(`{"repo":"example/project","prompt":"Task without attachments"}`))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	runBrowserSuite(t, ts.URL, time.Minute, "task asset checks", "tests/task_assets.cjs")
}

func TestTaskAssetPreviewDoesNotUseServerAuthorization(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("preview looked up server GitHub credentials")
		return execx.Result{Stdout: "fixture-token"}, nil
	}), true)
	srv.Broker.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Inspect files","assets":[
		{"url":"https://github.com/user-attachments/assets/private"},
		{"url":"https://private-user-images.githubusercontent.com/42/private.png?jwt=fixture"},
		{"url":"https://github.com/user-attachments/assets/private?token=fixture"}
	]}`))
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/user-attachments/assets/private" || req.URL.RawQuery != "" {
			t.Error("preview fetched a stored capability URL")
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Error("preview forwarded authorization")
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("%PDF-1.4\nprivate bytes"))}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
	})

	handler := srv.Handler()
	for index, tc := range []struct {
		url    string
		status int
	}{
		{"https://github.com/user-attachments/assets/private", http.StatusForbidden},
		{"https://private-user-images.githubusercontent.com/42/private.png?jwt=fixture", http.StatusBadGateway},
		{"https://github.com/user-attachments/assets/private?token=fixture", http.StatusBadGateway},
	} {
		w := httptest.NewRecorder()
		req := taskAssetPreviewRequest("req-assets", tc.url)
		handler.ServeHTTP(w, req)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "private bytes") {
			t.Errorf("preview %d returned status %d, want %d without attachment bytes", index, w.Code, tc.status)
		}
	}
}

func TestTaskAssetPreviewUsesViewerAuthorization(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("preview looked up server GitHub credentials")
		return execx.Result{}, fmt.Errorf("unexpected GitHub CLI command")
	}), true)
	srv.Broker.RecordTaskRunConfig("req-private", []byte(`{"assets":[
		{"url":"https://github.com/user-attachments/assets/private"},
		{"url":"https://private-user-images.githubusercontent.com/42/private.png?jwt=stored-fixture"},
		{"url":"https://github.com/user-attachments/assets/private?token=stored-fixture"}
	]}`))
	const privateBody = "%PDF-1.4\nprivate bytes"
	var storageRequests int
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Cookie") != "" {
			t.Error("preview forwarded cookies")
		}
		switch req.URL.String() {
		case "https://github.com/user-attachments/assets/private":
			if req.Header.Get("Authorization") == "Bearer viewer-fixture" {
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{
					"Location": {"https://private-user-images.githubusercontent.com/42/private.png?jwt=fresh-fixture"},
				}, Body: http.NoBody}, nil
			}
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
		case "https://private-user-images.githubusercontent.com/42/private.png?jwt=fresh-fixture":
			storageRequests++
			if req.Header.Get("Authorization") != "" {
				t.Error("viewer token forwarded to storage")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(privateBody))}, nil
		default:
			t.Error("preview fetched an unexpected URL")
			return nil, fmt.Errorf("unexpected attachment request")
		}
	})
	handler := srv.Handler()
	for _, tc := range []struct {
		name, authorization, assetURL string
		status                        int
	}{
		{"other viewer", "Bearer denied-fixture", "https://github.com/user-attachments/assets/private", http.StatusForbidden},
		{"authorized viewer", "Bearer viewer-fixture", "https://github.com/user-attachments/assets/private", http.StatusOK},
		{"anonymous after authorized", "", "https://github.com/user-attachments/assets/private", http.StatusForbidden},
		{"stored storage URL", "Bearer viewer-fixture", "https://private-user-images.githubusercontent.com/42/private.png?jwt=stored-fixture", http.StatusBadGateway},
		{"stored URL with query", "Bearer viewer-fixture", "https://github.com/user-attachments/assets/private?token=stored-fixture", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := taskAssetPreviewRequest("req-private", tc.assetURL)
			req.Header.Set("Authorization", tc.authorization)
			req.AddCookie(&http.Cookie{Name: "viewer", Value: "cookie-fixture"})
			handler.ServeHTTP(w, req)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("preview status = %d, cache control = %q, want %d and no-store", w.Code, w.Header().Get("Cache-Control"), tc.status)
			}
			if (w.Body.String() == privateBody) != (tc.status == http.StatusOK) {
				t.Fatal("unexpected private attachment access")
			}
		})
	}
	native := httptest.NewRequest(http.MethodGet, "/api/tasks/req-private/asset-preview?video="+
		taskAssetVideoID("https://github.com/user-attachments/assets/private"), nil)
	native.Header.Set("Authorization", "Bearer viewer-fixture")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, native)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), privateBody) {
		t.Fatalf("native playback forwarded viewer authorization: status = %d", w.Code)
	}
	if storageRequests != 1 {
		t.Fatalf("storage requests = %d, want one viewer-authorized request", storageRequests)
	}
}

func TestTaskAssetPreviewDistinguishesAccessFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		upstream, want int
		header         http.Header
	}{
		{name: "unauthorized", upstream: 401, want: 403},
		{name: "forbidden", upstream: 403, want: 403},
		{name: "missing or private", upstream: 404, want: 403},
		{name: "GitHub sign-in redirect", upstream: 302, want: 403, header: http.Header{"Location": {"/login"}}},
		{name: "network failure", want: 502},
		{name: "upstream failure", upstream: 503, want: 502},
		{name: "unsupported media", upstream: 200, want: 502},
		{name: "oversized asset", upstream: 200, want: 502},
		{name: "rate limited", upstream: 403, want: 502, header: http.Header{"Retry-After": {"60"}}},
		{name: "storage denied", upstream: 302, want: 502, header: http.Header{"Location": {"https://objects.githubusercontent.com/asset?signature=fixture"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := githubIssueTestServer(nil, true)
			srv.Broker.RecordTaskRunConfig("req-asset", []byte(`{"assets":[{"url":"https://github.com/user-attachments/assets/example"}]}`))
			srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
				if tc.upstream == 0 {
					return nil, fmt.Errorf("upstream unavailable")
				}
				if req.URL.Host == "objects.githubusercontent.com" {
					return &http.Response{StatusCode: http.StatusForbidden, Request: req, Body: http.NoBody}, nil
				}
				if req.URL.Path != "/user-attachments/assets/example" {
					t.Errorf("unexpected upstream path %q", req.URL.Path)
				}
				resp := &http.Response{StatusCode: tc.upstream, Request: req, Header: tc.header, Body: io.NopCloser(strings.NewReader("\x00\xff"))}
				if tc.name == "oversized asset" {
					resp.ContentLength = 26 << 20
				}
				return resp, nil
			})
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, taskAssetPreviewRequest("req-asset", "https://github.com/user-attachments/assets/example"))
			if w.Code != tc.want || w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("preview status = %d, cache control = %q, want %d and no-store", w.Code, w.Header().Get("Cache-Control"), tc.want)
			}
		})
	}
}

func TestTaskAssetPreviewStreamsVideo(t *testing.T) {
	t.Parallel()
	const assetURL = "https://github.com/user-attachments/assets/video"
	prefix := "\x00\x00\x00\x14ftypmp42\x00\x00\x00\x00mp42" + strings.Repeat("\x00", 512-20)
	for _, tc := range []struct {
		name        string
		upstreamErr error
	}{
		{name: "success"},
		{name: "interrupted", upstreamErr: errors.New("upstream interrupted")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := githubIssueTestServer(nil, true)
			srv.Broker.RecordTaskRunConfig("req-video", []byte(fmt.Sprintf(`{"assets":[{"url":%q}]}`, assetURL)))
			upstream, finish := io.Pipe()
			srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: struct {
					io.Reader
					io.Closer
				}{io.MultiReader(strings.NewReader(prefix), upstream), upstream}}, nil
			})
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			defer finish.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/tasks/req-video/asset-preview", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Task-Asset-URL", url.PathEscape(assetURL))
			req.Header.Set("Range", "bytes=0-1")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("unexpected video response: status = %d, headers = %v", resp.StatusCode, resp.Header)
			}
			if resp.Header.Get("Accept-Ranges") != "" || resp.Header.Get("Content-Range") != "" {
				t.Fatalf("unknown-length stream advertised range support: %v", resp.Header)
			}
			data := make([]byte, len(prefix))
			if _, err := io.ReadFull(resp.Body, data); err != nil || string(data) != prefix {
				t.Fatalf("first video bytes were not delivered before upstream EOF: %v", err)
			}
			finish.CloseWithError(tc.upstreamErr)
			if _, err := io.Copy(io.Discard, resp.Body); (err != nil) != (tc.upstreamErr != nil) {
				t.Fatalf("client read error = %v, upstream read error = %v", err, tc.upstreamErr)
			}
		})
	}
}

func TestTaskAssetPreviewVideoRanges(t *testing.T) {
	t.Parallel()
	const assetURL = "https://github.com/user-attachments/assets/video"
	const data = "\x00\x00\x00\x14ftypmp42\x00\x00\x00\x00mp42video payload"
	srv := githubIssueTestServer(nil, true)
	srv.Broker.RecordTaskRunConfig("req-video", []byte(fmt.Sprintf(`{"assets":[{"url":%q}]}`, assetURL)))
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Body: io.NopCloser(strings.NewReader(data))}, nil
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, tc := range []struct {
		name, rangeHeader, ifRange, contentRange, body string
		status                                         int
	}{
		{"Safari probe", "bytes=0-1", "", "bytes 0-1/33", data[:2], http.StatusPartialContent},
		{"open end", "bytes=20-", "", "bytes 20-32/33", data[20:], http.StatusPartialContent},
		{"suffix", "bytes=-7", "", "bytes 26-32/33", data[26:], http.StatusPartialContent},
		{"clamped end", "bytes=30-99", "", "bytes 30-32/33", data[30:], http.StatusPartialContent},
		{"unsatisfiable", "bytes=33-", "", "bytes */33", "", http.StatusRequestedRangeNotSatisfiable},
		{"no range", "", "", "", data, http.StatusOK},
		{"unmatched validator", "bytes=0-1", `"old"`, "", data, http.StatusOK},
		{"multiple ranges", "bytes=0-1,4-5", "", "", data, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/tasks/req-video/asset-preview?video="+taskAssetVideoID(assetURL), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Range", tc.rangeHeader)
			req.Header.Set("If-Range", tc.ifRange)
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status || resp.Header.Get("Content-Range") != tc.contentRange {
				t.Fatalf("got status %d and Content-Range %q, want %d and %q", resp.StatusCode, resp.Header.Get("Content-Range"), tc.status, tc.contentRange)
			}
			if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Content-Encoding") != "" {
				t.Fatalf("unexpected preview headers: %v", resp.Header)
			}
			if tc.status == http.StatusRequestedRangeNotSatisfiable {
				return
			}
			if resp.Header.Get("Content-Type") != "video/mp4" || tc.status == http.StatusPartialContent && resp.ContentLength != int64(len(tc.body)) {
				t.Fatalf("unexpected video headers: %v", resp.Header)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != tc.body {
				t.Fatalf("got body %q and error %v, want %q", body, err, tc.body)
			}
		})
	}
}

func TestTaskAssetPreviewFetchesUpstreamVideoRange(t *testing.T) {
	t.Parallel()
	const assetURL = "https://github.com/user-attachments/assets/video"
	const prefix = "\x00\x00\x00\x14ftypmp42\x00\x00\x00\x00mp42"
	const data = "video"
	for _, tc := range []struct {
		name, contentRange, encoding string
		status                       int
	}{
		{"seek", "bytes 4096-4100/8192", "", http.StatusPartialContent},
		{"wrong range", "bytes 0-4/8192", "", http.StatusBadGateway},
		{"wrong total", "bytes 4096-4100/16384", "", http.StatusBadGateway},
		{"encoded range", "bytes 4096-4100/8192", "gzip", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := githubIssueTestServer(nil, true)
			srv.Broker.RecordTaskRunConfig("req-video", []byte(fmt.Sprintf(`{"assets":[{"url":%q}]}`, assetURL)))
			srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
					t.Error("native seek forwarded viewer credentials")
				}
				if req.URL.Host == "github.com" {
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{
						"Location": {"https://objects.githubusercontent.com/video?signature=fixture"},
					}, Body: http.NoBody}, nil
				}
				if req.URL.Host != "objects.githubusercontent.com" {
					return nil, fmt.Errorf("unexpected seek host")
				}
				if req.Header.Get("Range") == "" {
					// Provide just the sniff window. Reading through to the seek
					// offset would hit EOF before the requested bytes.
					return &http.Response{StatusCode: http.StatusOK, ContentLength: 8192,
						Body: io.NopCloser(strings.NewReader(prefix + strings.Repeat("\x00", 512-len(prefix))))}, nil
				}
				if req.Header.Get("Range") != "bytes=4096-4100" || req.Header.Get("Accept-Encoding") != "identity" {
					t.Error("upstream did not receive the exact byte range with identity encoding")
				}
				return &http.Response{StatusCode: http.StatusPartialContent, ContentLength: int64(len(data)), Header: http.Header{
					"Content-Range": {tc.contentRange}, "Content-Encoding": {tc.encoding},
				}, Body: io.NopCloser(strings.NewReader(data))}, nil
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/tasks/req-video/asset-preview?video="+taskAssetVideoID(assetURL), nil)
			req.Header.Set("Range", "bytes=4096-4100")
			req.Header.Set("Accept-Encoding", "gzip")
			req.Header.Set("Authorization", "Bearer viewer-fixture")
			req.AddCookie(&http.Cookie{Name: "viewer", Value: "cookie-fixture"})
			srv.Handler().ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("seek status = %d, want %d", w.Code, tc.status)
			}
			if tc.status == http.StatusPartialContent && (w.Body.String() != data ||
				w.Header().Get("Content-Range") != tc.contentRange || w.Header().Get("Content-Encoding") != "") {
				t.Fatalf("unexpected seek body or headers: %q, %v", w.Body.String(), w.Header())
			}
		})
	}
}

func taskAssetTestPDF() string {
	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 240] /Contents 4 0 R >>",
		"<< /Length 29 >>\nstream\n0 0.3 0.5 rg 0 0 300 240 re f\nendstream",
	}
	var offsets []int
	for i, object := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return pdf.String()
}

func taskAssetTestWAV() string {
	const samples = 8000
	// One second of 8 kHz, 8-bit mono PCM.
	data := make([]byte, 44+samples)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], 36+samples)
	copy(data[8:], "WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00")
	binary.LittleEndian.PutUint32(data[24:], samples)
	binary.LittleEndian.PutUint32(data[28:], samples)
	copy(data[32:], "\x01\x00\x08\x00data")
	binary.LittleEndian.PutUint32(data[40:], samples)
	for i := range data[44:] {
		data[44+i] = 128
	}
	return string(data)
}

func taskAssetPreviewRequest(requestID, assetURL string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+requestID+"/asset-preview", nil)
	req.Header.Set("X-Task-Asset-URL", url.PathEscape(assetURL))
	return req
}

func TestTaskAssetPreviewRequiresStoredAsset(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("unexpected credential lookup")
		return execx.Result{}, nil
	}), true)
	srv.Broker.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Inspect files","assets":[{"url":"https://github.com/user-attachments/files/42/spec.pdf"}]}`))
	handler := srv.Handler()
	for _, req := range []*http.Request{
		taskAssetPreviewRequest("missing", "https://github.com/user-attachments/files/42/spec.pdf"),
		taskAssetPreviewRequest("req-assets", "https://github.com/user-attachments/files/42/other.pdf"),
		httptest.NewRequest(http.MethodGet, "/api/tasks/req-assets/asset-preview?index=0", nil),
		httptest.NewRequest(http.MethodGet, "/api/tasks/req-assets/asset-preview?url=https://github.com/user-attachments/files/42/spec.pdf", nil),
		httptest.NewRequest(http.MethodGet, "/api/tasks/req-assets/asset-preview?video="+taskAssetVideoID("https://github.com/user-attachments/files/42/other.pdf"), nil),
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", req.URL, w.Code)
		}
	}
}

func TestTaskAssetPreviewFollowsIdentityAcrossSnapshots(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(nil, true)
	const selected = "https://github.com/user-attachments/files/42/selected%20file.txt"
	const other = "https://github.com/user-attachments/files/42/other.txt"
	record := func(assets string) {
		srv.Broker.RecordTaskRunConfig("req-assets", []byte(`{"assets":[`+assets+`]}`))
	}
	selectedAsset, otherAsset := fmt.Sprintf(`{"url":%q}`, selected), fmt.Sprintf(`{"url":%q}`, other)
	record(selectedAsset + "," + otherAsset)
	task, _ := srv.Broker.Task("req-assets")
	assetURL := task.Assets[0].URL
	fetches := 0
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		fetches++
		if req.URL.String() != selected {
			t.Errorf("preview fetched a different attachment")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("Selected attachment"))}, nil
	})
	handler := srv.Handler()
	record(otherAsset + "," + selectedAsset)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, taskAssetPreviewRequest("req-assets", assetURL))
	if w.Code != http.StatusOK || w.Body.String() != "Selected attachment" {
		t.Fatalf("reordered attachment status = %d, body = %q", w.Code, w.Body.String())
	}
	record(otherAsset)
	w = httptest.NewRecorder()
	req := taskAssetPreviewRequest("req-assets", assetURL)
	req.Header.Set("Authorization", "Bearer viewer-fixture")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || fetches != 1 {
		t.Fatalf("removed attachment status = %d, upstream requests = %d, want 404 and one upstream request total", w.Code, fetches)
	}
}

func TestTaskAssetsSurviveSnapshotsWithoutSharingSlices(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	b.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Inspect files","assets":[{"url":"https://github.com/user-attachments/files/42/spec.pdf","name":"spec.pdf","source":"Comment 1"}]}`))
	snapshot := b.Snapshot()
	if len(snapshot.Tasks) != 1 || len(snapshot.Tasks[0].Assets) != 1 {
		t.Fatalf("missing assets: %+v", snapshot.Tasks)
	}
	snapshot.Tasks[0].Assets[0].Name = "changed"
	task, ok := b.Task("req-assets")
	if !ok || len(task.Assets) != 1 || task.Assets[0].Name != "spec.pdf" {
		t.Fatalf("snapshot mutated stored assets: %+v", task)
	}
	task.Assets[0].Name = "changed again"
	task, _ = b.Task("req-assets")
	if task.Assets[0].Name != "spec.pdf" {
		t.Fatal("Task returned mutable assets")
	}
	b.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Attachment removed"}`))
	task, _ = b.Task("req-assets")
	if len(task.Assets) != 0 {
		t.Fatal("removed assets persisted")
	}
}
