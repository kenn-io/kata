package web

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/klauspost/compress/gzhttp"
)

const (
	defaultRoute = "/kata"
	webCSP       = "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'"
)

var fixedContentTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".gif":         "image/gif",
	".html":        "text/html; charset=utf-8",
	".ico":         "image/x-icon",
	".jpeg":        "image/jpeg",
	".jpg":         "image/jpeg",
	".js":          "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".png":         "image/png",
	".svg":         "image/svg+xml",
	".webmanifest": "application/manifest+json",
	".webp":        "image/webp",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
}

type handler struct {
	files   fs.FS
	index   []byte
	assets  assetCatalog
	wrapped http.Handler
}

// NewHandler constructs the hardened static and navigation-fallback handler.
func NewHandler(files fs.FS) (http.Handler, error) {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read embedded web index: %w", err)
	}
	catalog, err := loadAssetCatalog(files)
	if err != nil {
		return nil, err
	}
	h := &handler{files: files, index: index, assets: catalog}
	wrap, err := gzhttp.NewWrapper(
		gzhttp.MinSize(0),
		gzhttp.ContentTypes([]string{
			"text/html", "text/javascript", "application/javascript", "text/css",
			"image/svg+xml", "application/json", "application/manifest+json",
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("configure web compression: %w", err)
	}
	h.wrapped = wrap(http.HandlerFunc(h.serve))
	return h, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.wrapped.ServeHTTP(w, r)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/" {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, defaultRoute, http.StatusFound)
		return
	}
	if r.URL.Path == defaultRoute+"/" {
		// Relative assets must resolve from the origin root on standalone pages.
		location := (&url.URL{Path: "../kata", RawQuery: r.URL.RawQuery}).String()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	if daemonOwnedPath(r.URL.Path) || !safeRequestPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/")
	data, err := fs.ReadFile(h.files, name)
	if err == nil {
		info, statErr := fs.Stat(h.files, name)
		if statErr != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		h.serveFile(w, r, name, data)
		return
	}
	if !errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if !navigationRequest(r) || dottedPath(name) {
		http.NotFound(w, r)
		return
	}
	h.serveHTML(w, r)
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	contentType := contentTypeFor(name)
	w.Header().Set("Content-Type", contentType)
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else if h.assets.isImmutable(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	writeStaticBody(w, r, data)
}

func (h *handler) serveHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", fixedContentTypes[".html"])
	w.Header().Set("Cache-Control", "no-store")
	writeStaticBody(w, r, h.index)
}

func writeStaticBody(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data) //nolint:gosec // G705: this handler intentionally serves validated static distribution bytes.
}

func setSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", webCSP)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
}

func contentTypeFor(name string) string {
	if value, ok := fixedContentTypes[strings.ToLower(path.Ext(name))]; ok {
		return value
	}
	return "application/octet-stream"
}

func daemonOwnedPath(value string) bool {
	return value == "/api" || strings.HasPrefix(value, "/api/") ||
		value == "/openapi.yaml" || value == "/openapi.json"
}

func safeRequestPath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.Contains(value, `\`) || strings.ContainsRune(value, '\x00') {
		return false
	}
	cleaned := path.Clean(value)
	if cleaned != value && cleaned+"/" != value {
		return false
	}
	for segment := range strings.SplitSeq(strings.TrimPrefix(value, "/"), "/") {
		if segment != "" && unsafePathSegment(segment) {
			return false
		}
	}
	return true
}

func navigationRequest(r *http.Request) bool {
	for value := range strings.SplitSeq(r.Header.Get("Accept"), ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err == nil && (mediaType == "text/html" || mediaType == "application/xhtml+xml") {
			return true
		}
	}
	return false
}

func dottedPath(name string) bool {
	for segment := range strings.SplitSeq(name, "/") {
		if strings.Contains(segment, ".") {
			return true
		}
	}
	return false
}
