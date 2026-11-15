package mcpserver

import (
	"net/http"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPOptions controls transport protections shared by both protocol paths.
type HTTPOptions struct {
	// DisableLocalhostProtection delegates Host validation to the enclosing listener.
	DisableLocalhostProtection bool
}

// IsModernRequest selects the stateless protocol path, even with a legacy session ID.
func IsModernRequest(r *http.Request) bool {
	return r.Header.Get("MCP-Protocol-Version") >= "2026-07-28"
}

// NewHTTPHandler serves legacy sessions from legacy and 2026-07-28 stateless
// requests from current on the same endpoint. Events is served only by
// current: build legacy without Options.Events so it neither registers nor
// advertises it, and Events methods there answer method-not-found. The SDK
// validates the required header/metadata agreement; the Mcp-Name guard
// covers what the SDK does not validate for Events. Authentication, Host
// validation and cross-origin protection remain the caller's responsibility
// so both protocol paths share the same transport authority.
func NewHTTPHandler(legacy, current func(*http.Request) *sdkmcp.Server, options HTTPOptions) http.Handler {
	legacyHandler := sdkmcp.NewStreamableHTTPHandler(legacy, &sdkmcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute, PropagateRequestCancellation: true,
		DisableLocalhostProtection: options.DisableLocalhostProtection,
	})
	currentHandler := eventsNameHeaderHandler(sdkmcp.NewStreamableHTTPHandler(current, &sdkmcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true,
		DisableLocalhostProtection: options.DisableLocalhostProtection,
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsModernRequest(r) {
			currentHandler.ServeHTTP(w, r)
			return
		}
		legacyHandler.ServeHTTP(w, r)
	})
}
