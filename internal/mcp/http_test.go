package mcpserver

import (
	"context"
	"net"
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestHTTPHandlerHostPolicy(t *testing.T) {
	_, client := newRecordingDaemon(t)
	server, err := New(Options{Client: client, Scope: boundTestScope(t), Actor: "example-agent", Version: "test", Events: true})
	require.NoError(t, err)
	selector := func(*http.Request) *sdkmcp.Server { return server }
	for disable, want := range map[bool]int{false: http.StatusForbidden, true: http.StatusOK} {
		request := currentRequest("server/discover", "", eventsRPC("server/discover", ""))
		request = request.WithContext(context.WithValue(t.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777}))
		request.Host = "daemon.example"
		recorder, _ := serve(NewHTTPHandler(selector, selector, HTTPOptions{DisableLocalhostProtection: disable}), request)
		require.Equal(t, want, recorder.Code, recorder.Body.String())
	}
}
