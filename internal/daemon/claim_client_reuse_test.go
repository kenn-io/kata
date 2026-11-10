package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// Measure the existing HTTP transport before changing client construction.
func TestClaimClientTransportReuse(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	for range 2 {
		client, err := newClaimHubClient(t.Context(), server.URL, "test-token", true, nil)
		require.NoError(t, err)
		_, _ = client.ClaimStatus(t.Context(), 1, "01K00000000000000000000000")
	}
	require.EqualValues(t, 1, connections.Load(), "fresh clients already share the default transport's keep-alive connections")
	t.Logf("two independent claim clients used %d TCP connection(s)", connections.Load())
}
