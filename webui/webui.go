// Package webui embeds Kata's optional browser application. API-only hosts
// can import go.kenn.io/kata without linking these assets.
package webui

import (
	"net/http"

	"go.kenn.io/kata/internal/web"
)

// NewEmbeddedHandler serves the browser application bundled with this module.
// Pass it as kata.Config.WebHandler to include it in an embedded service.
func NewEmbeddedHandler() (http.Handler, error) {
	return web.NewEmbeddedHandler()
}
