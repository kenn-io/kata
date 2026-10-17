package daemonmcp

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTransportStreamsAndCancelsOnClose(t *testing.T) {
	done := make(chan struct{})
	transport := requestTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		require.Equal(t, "Bearer caller", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Empty(t, r.Header.Get("X-Forwarded-Host"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: event\n\n")
		<-r.Context().Done()
	}), headers: http.Header{"Authorization": {"Bearer caller"}}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", "http://mcp.internal/api/v1/events/stream", nil)
	require.NoError(t, err)
	request.Header.Set("Cookie", "ambient=secret")
	request.Header.Set("X-Forwarded-Host", "other.example")
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "data: event\n", line)
	require.NoError(t, response.Body.Close())
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("response close did not cancel REST handler")
	}
}

func TestTransportPreservesConditionalHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	transport := requestTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.WriteHeader(204)
	})}
	req, err := http.NewRequest("POST", "http://mcp.internal/api/v1/projects/1/issues", nil)
	require.NoError(t, err)
	for _, key := range []string{"If-Match", "Idempotency-Key", "X-Kata-Confirm"} {
		req.Header.Set(key, "required-value")
	}
	response, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_ = response.Body.Close()
	headers := <-got
	for _, key := range []string{"If-Match", "Idempotency-Key", "X-Kata-Confirm"} {
		require.Equal(t, "required-value", headers.Get(key), key)
	}
}

func TestTransportCancellationBeforeHeaders(t *testing.T) {
	transport := requestTransport{handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://mcp.internal/api/v1/projects", nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(req)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestTransportNeverDispatchesAnotherOrigin(t *testing.T) {
	transport := requestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unexpected dispatch")
	})}
	for _, target := range []string{"https://other.example/api/v1/projects", "http://mcp.internal/mcp"} {
		req, err := http.NewRequest("GET", target, nil)
		require.NoError(t, err)
		_, err = transport.RoundTrip(req)
		require.Error(t, err)
	}
}
