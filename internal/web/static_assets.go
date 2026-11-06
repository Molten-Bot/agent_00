package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// staticAssetRefPattern matches the src/href attributes of the embedded scripts
// and stylesheets a page pulls in, with or without a query already attached.
// Those are the files whose cached copy has to match the markup that shipped
// with them; a stale image is just an old picture.
var staticAssetRefPattern = regexp.MustCompile(`(src|href)="/static/([^"?#]+\.(?:css|js))(?:\?[^"#]*)?"`)

// staticAssetVersions caches each embedded asset's cache-busting token. The
// embedded bytes cannot change while the process runs, so one hash per file is
// enough.
var staticAssetVersions sync.Map // name -> string

// withStaticAssetVersions stamps every /static/ reference in an HTML page with
// a query derived from that file's bytes. Static assets are served with
// public, max-age=3600, so without it a browser can pair fresh markup with an
// hour-old stylesheet or script. A hand-written ?v=N only closes that window
// when somebody remembers to bump it; deriving the query from the content
// makes the bump automatic and leaves unchanged files cacheable.
func withStaticAssetVersions(page []byte) []byte {
	return staticAssetRefPattern.ReplaceAllFunc(page, func(match []byte) []byte {
		groups := staticAssetRefPattern.FindSubmatch(match)
		version := staticAssetVersion(string(groups[2]))
		if version == "" {
			return match
		}
		return []byte(string(groups[1]) + `="/static/` + string(groups[2]) + `?v=` + version + `"`)
	})
}

// staticAssetVersion returns a short content hash for a path under static/, or
// an empty string when it names no embedded file. The hash shares its prefix
// with the ETag the static handler serves, because both hash the same bytes.
func staticAssetVersion(name string) string {
	if cached, ok := staticAssetVersions.Load(name); ok {
		return cached.(string)
	}
	version := ""
	// The leading slash keeps a ".." segment from climbing out of static/.
	if data, err := fs.ReadFile(staticFiles, path.Join("static", path.Clean("/"+name))); err == nil {
		sum := sha256.Sum256(data)
		version = hex.EncodeToString(sum[:6])
	}
	cached, _ := staticAssetVersions.LoadOrStore(name, version)
	return cached.(string)
}

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
