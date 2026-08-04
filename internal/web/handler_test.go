package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebHandlerAssetsAndNavigationFallback(t *testing.T) {
	files := webHandlerTestFS(t)
	handler, err := NewHandler(files)
	require.NoError(t, err)

	t.Run("root redirect", func(t *testing.T) {
		response := serveWebRequest(handler, http.MethodGet, "/", "text/html", "")
		assert.Equal(t, http.StatusFound, response.Code)
		assert.Equal(t, "/kata", response.Header().Get("Location"))
	})

	t.Run("deep link", func(t *testing.T) {
		response := serveWebRequest(handler, http.MethodGet, "/kata?issue=01HZNQ7VFPK1XGD8R5MABCD4EX", "text/html", "")
		require.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, string(files["index.html"].Data), response.Body.String())
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
		assert.Contains(t, response.Header().Get("Content-Security-Policy"), "default-src 'self'")
		assert.Contains(t, response.Header().Get("Content-Security-Policy"), "style-src-attr 'unsafe-inline'")
	})

	t.Run("non-navigation miss", func(t *testing.T) {
		response := serveWebRequest(handler, http.MethodGet, "/issues/missing", "application/json", "")
		assert.Equal(t, http.StatusNotFound, response.Code)
	})

	for _, path := range []string{
		"/api/v1/issues",
		"/api/v1/ping",
		"/api/v1/health",
		"/openapi.yaml",
		"/openapi.json",
	} {
		t.Run("owned "+path, func(t *testing.T) {
			response := serveWebRequest(handler, http.MethodGet, path, "text/html", "")
			assert.Equal(t, http.StatusNotFound, response.Code)
			assert.NotContains(t, response.Body.String(), "Kata test shell")
		})
	}
}

func TestWebHandlerExactAssetsCachingAndMIME(t *testing.T) {
	files := webHandlerTestFS(t)
	handler, err := NewHandler(files)
	require.NoError(t, err)

	for _, tt := range []struct {
		path        string
		contentType string
		immutable   bool
	}{
		{path: "/assets/app-a1b2c3d4.js", contentType: "text/javascript; charset=utf-8", immutable: true},
		{path: "/assets/app-e5f6a7b8.css", contentType: "text/css; charset=utf-8", immutable: true},
		{path: "/assets/icon-aabbccdd.svg", contentType: "image/svg+xml", immutable: true},
		{path: "/assets/data-11223344.json", contentType: "application/json", immutable: true},
		{path: "/favicon.svg", contentType: "image/svg+xml", immutable: false},
	} {
		t.Run(tt.path, func(t *testing.T) {
			response := serveWebRequest(handler, http.MethodGet, tt.path, "*/*", "")
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, tt.contentType, response.Header().Get("Content-Type"))
			if tt.immutable {
				assert.Equal(t, "public, max-age=31536000, immutable", response.Header().Get("Cache-Control"))
			} else {
				assert.NotContains(t, response.Header().Get("Cache-Control"), "immutable")
			}
		})
	}
}

func TestWebHandlerRejectsUnsafeAndNonNavigationPaths(t *testing.T) {
	handler, err := NewHandler(webHandlerTestFS(t))
	require.NoError(t, err)

	for _, path := range []string{
		"/missing.js",
		"/.env",
		"/.vite/manifest.json",
		"/secrets.json",
		"/client.pem",
		"/credentials/token",
		"/views/inbox\\credentials",
		"/views/../index.html",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://daemon.example/", nil)
			request.URL.Path = path
			request.Header.Set("Accept", "text/html")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, http.StatusNotFound, response.Code)
			assert.NotContains(t, response.Body.String(), "Kata test shell")
		})
	}
}

func TestWebHandlerCompressesStaticTextFormats(t *testing.T) {
	files := webHandlerTestFS(t)
	handler, err := NewHandler(files)
	require.NoError(t, err)

	for _, tt := range []struct {
		path string
		want []byte
	}{
		{path: "/kata", want: files["index.html"].Data},
		{path: "/assets/app-a1b2c3d4.js", want: files["assets/app-a1b2c3d4.js"].Data},
		{path: "/assets/app-e5f6a7b8.css", want: files["assets/app-e5f6a7b8.css"].Data},
		{path: "/assets/icon-aabbccdd.svg", want: files["assets/icon-aabbccdd.svg"].Data},
		{path: "/assets/data-11223344.json", want: files["assets/data-11223344.json"].Data},
	} {
		t.Run(tt.path, func(t *testing.T) {
			response := serveWebRequest(handler, http.MethodGet, tt.path, "text/html", "gzip")
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, "gzip", response.Header().Get("Content-Encoding"))
			reader, err := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
			require.NoError(t, err)
			plain, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			assert.Equal(t, tt.want, plain)
		})
	}
}

func serveWebRequest(handler http.Handler, method, path, accept, encoding string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://daemon.example"+path, nil)
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	if encoding != "" {
		request.Header.Set("Accept-Encoding", encoding)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func webHandlerTestFS(t *testing.T) fstest.MapFS {
	t.Helper()
	manifest, err := json.Marshal(map[string]any{
		"src/main.ts": map[string]any{
			"file": "assets/app-a1b2c3d4.js",
			"css":  []string{"assets/app-e5f6a7b8.css"},
			"assets": []string{
				"assets/icon-aabbccdd.svg",
				"assets/data-11223344.json",
			},
		},
	})
	require.NoError(t, err)
	return fstest.MapFS{
		"index.html":                   &fstest.MapFile{Data: []byte("<!doctype html><title>Kata test shell</title>" + strings.Repeat(" safe", 80)), Mode: fs.FileMode(0o444)},
		".vite/manifest.json":          &fstest.MapFile{Data: manifest, Mode: fs.FileMode(0o444)},
		"assets/app-a1b2c3d4.js":       &fstest.MapFile{Data: []byte(strings.Repeat("export const ready=true;", 20)), Mode: fs.FileMode(0o444)},
		"assets/app-e5f6a7b8.css":      &fstest.MapFile{Data: []byte(strings.Repeat("body{color:#123456}", 20)), Mode: fs.FileMode(0o444)},
		"assets/icon-aabbccdd.svg":     &fstest.MapFile{Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h8v8z"/></svg>`), Mode: fs.FileMode(0o444)},
		"assets/data-11223344.json":    &fstest.MapFile{Data: []byte(`{"ready":true}`), Mode: fs.FileMode(0o444)},
		"favicon.svg":                  &fstest.MapFile{Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), Mode: fs.FileMode(0o444)},
		".env":                         &fstest.MapFile{Data: []byte("not served"), Mode: fs.FileMode(0o444)},
		"client.pem":                   &fstest.MapFile{Data: []byte("not served"), Mode: fs.FileMode(0o444)},
		"credentials/token":            &fstest.MapFile{Data: []byte("not served"), Mode: fs.FileMode(0o444)},
		"assets/unreferenced-file.txt": &fstest.MapFile{Data: []byte("public exact file"), Mode: fs.FileMode(0o444)},
	}
}
