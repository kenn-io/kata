package mcpserver

import (
	"context"
	"net"
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestHTTPHandlerRequestSelectorsAndHostPolicy(t *testing.T) {
	_, client := newRecordingDaemon(t)
	for _, disable := range []bool{false, true} {
		for _, version := range []string{"2025-11-25", "2026-07-28"} {
			options := Options{Client: client, Scope: boundTestScope(t), Actor: "example-agent", Version: "test"}
			legacy, err := New(options)
			require.NoError(t, err)
			options.Events = true
			current, err := New(options)
			require.NoError(t, err)
			selected := ""
			selector := func(name string, server *sdkmcp.Server) func(*http.Request) *sdkmcp.Server {
				return func(r *http.Request) *sdkmcp.Server { selected = r.Header.Get("X-Caller") + name; return server }
			}
			handler := NewHTTPHandler(selector("legacy", legacy), selector("modern", current), HTTPOptions{DisableLocalhostProtection: disable})
			request := currentRequest("server/discover", "", eventsRPC("server/discover", ""))
			if version == "2025-11-25" {
				request = currentRequest("initialize", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"test"}}}`)
			}
			request = request.WithContext(context.WithValue(t.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777}))
			request.Host = "daemon.example"
			request.Header.Set("X-Caller", "caller-")
			request.Header.Set("Mcp-Protocol-Version", version)
			recorder, _ := serve(handler, request)
			if !disable {
				require.Equal(t, http.StatusForbidden, recorder.Code)
			} else {
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				want := "caller-legacy"
				if version == "2026-07-28" {
					want = "caller-modern"
				}
				require.Equal(t, want, selected)
			}
		}
	}
}
