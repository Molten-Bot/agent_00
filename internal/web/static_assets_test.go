package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticAssetsServeCachedGzipWithETag(t *testing.T) {
	t.Parallel()

	handler := NewServer("", nil).Handler()
	want, err := fs.ReadFile(staticFiles, "static/site-header.js")
	if err != nil {
		t.Fatalf("read embedded asset: %v", err)
	}

	plainReq := httptest.NewRequest(http.MethodGet, "/static/site-header.js", nil)
	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, plainReq)
	if plain.Code != http.StatusOK {
		t.Fatalf("plain status = %d", plain.Code)
	}
	if !bytes.Equal(plain.Body.Bytes(), want) {
		t.Fatal("plain body differs from embedded asset")
	}
	if enc := plain.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("plain Content-Encoding = %q, want none", enc)
	}
	if ct := plain.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("plain Content-Type = %q, want javascript", ct)
	}
	if cc := plain.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	plainETag := plain.Header().Get("ETag")
	if plainETag == "" {
		t.Fatal("plain response missing ETag")
	}

	gzReq := httptest.NewRequest(http.MethodGet, "/static/site-header.js", nil)
	gzReq.Header.Set("Accept-Encoding", "gzip, br")
	gzRes := httptest.NewRecorder()
	handler.ServeHTTP(gzRes, gzReq)
	if gzRes.Code != http.StatusOK {
		t.Fatalf("gzip status = %d", gzRes.Code)
	}
	if enc := gzRes.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q", enc)
	}
	if ct := gzRes.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("gzip Content-Type = %q, want javascript", ct)
	}
	if vary := gzRes.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("Vary = %q", vary)
	}
	reader, err := gzip.NewReader(gzRes.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode gzip: %v", err)
	}
	if !bytes.Equal(decoded, want) {
		t.Fatal("decoded gzip body differs from embedded asset")
	}
	gzETag := gzRes.Header().Get("ETag")
	if gzETag == "" || gzETag == plainETag {
		t.Fatalf("gzip ETag = %q, want distinct from %q", gzETag, plainETag)
	}

	for _, tc := range []struct {
		etag     string
		encoding string
	}{{plainETag, ""}, {gzETag, "gzip"}} {
		req := httptest.NewRequest(http.MethodGet, "/static/site-header.js", nil)
		req.Header.Set("If-None-Match", tc.etag)
		if tc.encoding != "" {
			req.Header.Set("Accept-Encoding", tc.encoding)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match %s status = %d, want 304", tc.etag, res.Code)
		}
	}
}

func TestStaticAssetsStillServeAllPublishedURLs(t *testing.T) {
	t.Parallel()

	handler := NewServer("", nil).Handler()
	for _, path := range []string{"/static/xterm.js", "/static/xterm.css", "/static/emoji-picker.js", "/static/logo.svg", "/static/emoji-mart-data.json", "/static/marked.umd.js", "/static/marked-LICENSE.txt"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("%s %s status = %d", method, path, res.Code)
			}
			if method == http.MethodHead && res.Body.Len() != 0 {
				t.Fatalf("HEAD %s wrote %d body bytes", path, res.Body.Len())
			}
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/static/does-not-exist.js", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", res.Code)
	}
}
