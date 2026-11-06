package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// versionedStaticRef matches a /static/ reference that carries a cache-busting
// query, so the unversioned refs this test rejects are the ones left over.
var versionedStaticRef = regexp.MustCompile(`"/static/([^"?#]+)\?v=([0-9a-f]+)"`)

// TestPagesStampStaticAssetsWithContentVersions covers the cache-busting query
// on every /static/ reference a served page carries. The point is that nobody
// maintains these by hand: the query comes from the file's bytes, so a
// CSS-only or JS-only edit cannot ship behind the hour of caching /static/
// asks for, and an unchanged file keeps its old query and stays cached. The
// ETag the static handler computes over the same bytes is the cross-check a
// hand-written ?v=N could not satisfy.
func TestPagesStampStaticAssetsWithContentVersions(t *testing.T) {
	t.Parallel()

	handler := NewServer("", nil).Handler()
	seen := map[string]string{}
	for _, page := range []string{"/", "/game"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, page, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", page, res.Code)
		}
		body := res.Body.String()

		refs := versionedStaticRef.FindAllStringSubmatch(body, -1)
		if len(refs) == 0 {
			t.Fatalf("%s references no versioned static asset", page)
		}
		for _, all := range staticAssetRefPattern.FindAllString(body, -1) {
			if !versionedStaticRef.MatchString(all) {
				t.Errorf("%s serves %s without a cache-busting version, so an edit to it can stay cached for an hour", page, all)
			}
		}

		for _, ref := range refs {
			name, version := ref[1], ref[2]
			seen[name] = version
			asset := httptest.NewRecorder()
			handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/static/"+name, nil))
			if asset.Code != http.StatusOK {
				t.Errorf("%s references /static/%s, which serves %d", page, name, asset.Code)
				continue
			}
			if etag := asset.Header().Get("ETag"); !strings.HasPrefix(etag, `"`+version) {
				t.Errorf("/static/%s is served as %s but referenced as ?v=%s; the version does not track the file's content", name, etag, version)
			}
		}
	}

	// The stylesheet and the issues script are the two files this UI edits most
	// and the two whose staleness is visible. xterm.js is named because it is
	// the one asset the page loads by assigning script.src from an inline
	// script, which an attribute-only rewrite would walk straight past.
	for _, name := range []string{"style.css", "github-issues.js", "xterm.js"} {
		if seen[name] == "" {
			t.Errorf("no page stamped /static/%s with a content version", name)
		}
	}
	if seen["style.css"] == seen["github-issues.js"] {
		t.Error("two different files share a version, so the query is not derived from their content")
	}
}

// TestEmbeddedVersionedPageStampsOnce guards the cost of the stamp. The
// embedded bytes cannot change while the process runs, so rewriting ~580 KB of
// index.html on every page view would buy nothing; the handler must serve a
// page that was rewritten once.
func TestEmbeddedVersionedPageStampsOnce(t *testing.T) {
	t.Parallel()

	first, err := embeddedVersionedPage("static/index.html")
	if err != nil {
		t.Fatalf("embeddedVersionedPage: %v", err)
	}
	second, err := embeddedVersionedPage("static/index.html")
	if err != nil {
		t.Fatalf("embeddedVersionedPage: %v", err)
	}
	if len(first) == 0 || &first[0] != &second[0] {
		t.Fatal("index.html is re-stamped on every call instead of once")
	}
	if _, err := embeddedVersionedPage("static/not-a-page.html"); err == nil {
		t.Fatal("embeddedVersionedPage reported a page that is not embedded")
	}
}

func TestStaticAssetVersionsIgnoreUnknownFiles(t *testing.T) {
	t.Parallel()

	page := []byte(`<link rel="stylesheet" href="/static/style.css"><script src="/static/not-embedded.js"></script>`)
	stamped := string(withStaticAssetVersions(page))
	if !strings.Contains(stamped, `href="/static/style.css?v=`+staticAssetVersion("style.css")+`"`) {
		t.Fatalf("stamped page = %q, want the stylesheet carrying its content version", stamped)
	}
	if !strings.Contains(stamped, `src="/static/not-embedded.js"`) {
		t.Fatalf("stamped page = %q, want a reference to a missing file left alone", stamped)
	}
	if got := staticAssetVersion("../server.go"); got != "" {
		t.Fatalf("staticAssetVersion(%q) = %q, want no version for a path outside static/", "../server.go", got)
	}
}

func TestStaticAssetsStillServeAllPublishedURLs(t *testing.T) {
	t.Parallel()

	handler := NewServer("", nil).Handler()
	for _, path := range []string{"/static/xterm.js", "/static/xterm.css", "/static/emoji-picker.js", "/static/logo.svg", "/static/emoji-mart-data.json"} {
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
