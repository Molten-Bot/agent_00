package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// staticAsset is an embedded file prepared once for repeated serving: a
// content-hash ETag for revalidation and, for compressible types, a gzip
// encoding produced a single time instead of on every request.
type staticAsset struct {
	data   []byte
	gzip   []byte
	etag   string
	gzETag string
}

// embeddedStaticHandler serves regular files from an embedded filesystem with
// ETags and cached gzip encodings. Anything else (directories, missing files,
// index.html redirects) falls through to http.FileServer so its behavior is
// unchanged.
type embeddedStaticHandler struct {
	files    fs.FS
	fallback http.Handler
	assets   sync.Map // name -> *staticAsset, or nil marker for fallback
}

func newEmbeddedStaticHandler(files fs.FS) *embeddedStaticHandler {
	return &embeddedStaticHandler{
		files:    files,
		fallback: http.FileServer(http.FS(files)),
	}
}

func (h *embeddedStaticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.fallback.ServeHTTP(w, r)
		return
	}
	asset := h.asset(r.URL.Path)
	if asset == nil {
		h.fallback.ServeHTTP(w, r)
		return
	}
	name := path.Base(r.URL.Path)
	if asset.gzip != nil {
		addVaryHeader(w.Header(), "Accept-Encoding")
		if requestWantsGzip(r) && r.Header.Get("Range") == "" {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("ETag", asset.gzETag)
			// ServeContent derives Content-Type from the name, so the gzip body
			// keeps the uncompressed file's type.
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(asset.gzip))
			return
		}
	}
	w.Header().Set("ETag", asset.etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(asset.data))
}

// asset returns the prepared asset for a request path relative to the static
// root, or nil when the request should be handled by the fallback server.
func (h *embeddedStaticHandler) asset(urlPath string) *staticAsset {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" || name == "." || path.Base(name) == "index.html" {
		return nil
	}
	if cached, ok := h.assets.Load(name); ok {
		asset, _ := cached.(*staticAsset)
		return asset
	}
	asset := loadStaticAsset(h.files, name)
	if asset == nil {
		// Only remember fallbacks for paths that exist (e.g. directories) so
		// arbitrary 404 probes cannot grow the cache.
		if _, err := fs.Stat(h.files, name); err != nil {
			return nil
		}
	}
	cached, _ := h.assets.LoadOrStore(name, asset)
	asset, _ = cached.(*staticAsset)
	return asset
}

func loadStaticAsset(files fs.FS, name string) *staticAsset {
	info, err := fs.Stat(files, name)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(data)
	tag := hex.EncodeToString(sum[:12])
	asset := &staticAsset{
		data: data,
		etag: `"` + tag + `"`,
	}
	if isCompressiblePath(name) {
		var buf bytes.Buffer
		gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err == nil {
			if _, err = gz.Write(data); err == nil {
				err = gz.Close()
			}
		}
		if err == nil {
			asset.gzip = buf.Bytes()
			asset.gzETag = `"` + tag + `-gzip"`
		}
	}
	return asset
}
