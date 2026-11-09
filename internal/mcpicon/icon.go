// Package mcpicon shares Kata's public mark between MCP metadata and listeners.
package mcpicon

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"net/http"
	"time"
)

// iconPNG is the canonical website favicon rendered at 256x256.
// Regenerate from the repository root with:
//
//	rsvg-convert -w 256 -h 256 docs/website/favicon.svg -o /tmp/kata-mcp-icon.png
//	magick /tmp/kata-mcp-icon.png -strip -define png:exclude-chunks=date,time internal/mcpicon/assets/icon.png
//
//go:embed assets/icon.png
var iconPNG []byte

// Source is a data URI usable by both stdio and HTTP clients without a public URL.
var Source = "data:image/png;base64," + base64.StdEncoding.EncodeToString(iconPNG)

// RegisterRoutes serves only the public image, outside MCP bearer authentication.
// Both aliases return PNG bytes, including /favicon.ico.
func RegisterRoutes(mux *http.ServeMux) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, "icon.png", time.Time{}, bytes.NewReader(iconPNG))
	})
	mux.Handle("GET /icon.png", handler)
	mux.Handle("GET /favicon.ico", handler)
}
