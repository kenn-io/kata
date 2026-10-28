package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAttachmentProxyCancelsWhenSessionEnds(t *testing.T) {
	for _, path := range []string{
		"/api/v1/projects/1/issues/abc4/attachments?name=shot.png",
		"/api/v1/projects/1/issues/abc4/attachments/links",
	} {
		t.Run(path, func(t *testing.T) { testAttachmentProxyCancellation(t, path) })
	}
}

func testAttachmentProxyCancellation(t *testing.T, path string) {
	t.Helper()
	started := make(chan struct{})
	canceled := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-stop:
		}
	}))
	t.Cleanup(upstream.Close)
	gateway := &webDaemonGateway{proxies: make(map[string]http.Handler)}
	proxy, err := gateway.proxy(resolvedWebDaemon{id: "example", baseURL: upstream.URL, allowInsecure: true})
	require.NoError(t, err)
	var active atomic.Bool
	active.Store(true)
	ctx := context.WithValue(t.Context(), webSessionContextKey{}, true)
	ctx = context.WithValue(ctx, webSessionRevalidationContextKey{}, func(context.Context) error {
		if !active.Load() {
			return errors.New("session ended")
		}
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("image")).WithContext(ctx)
	req.Header.Set("Content-Type", "application/octet-stream")
	done := make(chan struct{})
	go func() { proxy.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not reach target")
	}
	active.Store(false)
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Error("target upload was not canceled")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("proxy did not stop")
	}
}

type invalidateOnRead struct{ active *atomic.Bool }

func (r invalidateOnRead) Read(_ []byte) (int, error) { r.active.Store(false); return 0, io.EOF }
func TestAttachmentProxyRechecksAfterReceivingBody(t *testing.T) {
	var called atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	t.Cleanup(upstream.Close)
	gateway := &webDaemonGateway{proxies: make(map[string]http.Handler)}
	proxy, err := gateway.proxy(resolvedWebDaemon{id: "example", baseURL: upstream.URL, allowInsecure: true})
	require.NoError(t, err)
	var active atomic.Bool
	active.Store(true)
	ctx := context.WithValue(t.Context(), webSessionContextKey{}, true)
	ctx = context.WithValue(ctx, webSessionRevalidationContextKey{}, func(context.Context) error {
		if !active.Load() {
			return errors.New("session ended")
		}
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/1/issues/abc4/attachments?name=shot.png", invalidateOnRead{&active}).WithContext(ctx)
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, req)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.False(t, called.Load())
}
