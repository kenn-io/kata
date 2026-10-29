package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
)

func TestServeWaitsForActiveHandlerDuringShutdown(t *testing.T) {
	for _, tt := range []struct {
		name       string
		closeError bool
		timeout    time.Duration
	}{
		{name: "plain", timeout: 5 * time.Second},
		{name: "concurrent_close", closeError: true, timeout: time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })

			entered := make(chan struct{})
			release := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(entered)
				<-release
				w.WriteHeader(http.StatusNoContent)
			})
			server := &Server{handler: handler}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			serveListener := listener
			if tt.closeError {
				serveListener = closeErrorListener{Listener: listener}
			}
			go func() { done <- server.Serve(ctx, serveListener) }()

			responseDone := make(chan error, 1)
			go func() {
				response, err := http.Get("http://" + listener.Addr().String())
				if err == nil {
					_ = response.Body.Close()
				}
				responseDone <- err
			}()
			select {
			case <-entered:
			case <-time.After(tt.timeout):
				stacktest.Dump(t)
				t.Fatal("request did not enter handler")
			}

			cancel()
			select {
			case err := <-done:
				t.Fatalf("Serve returned before active handler completed: %v", err)
			case <-time.After(50 * time.Millisecond): //nolint:kennlint // deadlinetest: absence window; release holds the active handler, so Serve must not return
			}

			close(release)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(tt.timeout):
				stacktest.Dump(t)
				t.Fatal("Serve did not return after active handler completed")
			}
			require.NoError(t, <-responseDone)
		})
	}
}

type closeErrorListener struct {
	net.Listener
}

func (l closeErrorListener) Close() error {
	_ = l.Listener.Close()
	return net.ErrClosed
}

func TestServeReportsHandlerThatOutlivesShutdownTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	entered := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})
	server := &Server{handler: handler, shutdownTimeout: 20 * time.Millisecond} //nolint:kennlint // deadlinetest: the handler stays blocked; shutdown deadline expiry must prevent safe dependency cleanup
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			_ = response.Body.Close()
		}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		stacktest.Dump(t)
		t.Fatal("request did not enter handler")
	}
	cancel()
	select {
	case serveErr := <-done:
		require.ErrorIs(t, serveErr, ErrHTTPHandlersUnjoined)
	case <-time.After(time.Second):
		stacktest.Dump(t)
		t.Fatal("Serve did not report the unjoined handler")
	}
	close(release)
}

func TestServeListenersNotifiesAtFirstListenerExitBeforeHandlerDrain(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	second, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})
	server := &Server{handler: handler}
	stopping := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- server.ServeListenersWithStop(context.Background(), func() {
			close(stopping)
		},
			ListenerBinding{Listener: first, Policy: ListenerPolicy{Kind: ListenerSocket}},
			ListenerBinding{Listener: second, Policy: ListenerPolicy{Kind: ListenerSocket}},
		)
	}()
	go func() {
		response, requestErr := http.Get("http://" + second.Addr().String())
		if requestErr == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		stacktest.Dump(t)
		t.Fatal("request did not enter sibling handler")
	}

	require.NoError(t, first.Close())
	select {
	case <-stopping:
	case <-time.After(time.Second):
		stacktest.Dump(t)
		t.Fatal("listener exit did not notify root shutdown")
	}
	select {
	case err := <-done:
		t.Fatalf("ServeListeners returned before sibling handler drained: %v", err)
	case <-time.After(50 * time.Millisecond): //nolint:kennlint // deadlinetest: absence window; release holds the sibling handler, so ServeListeners must not return
	}
	close(release)
	require.True(t, errors.Is(<-done, net.ErrClosed))
}

func TestServeListenersDoesNotNotifyReadyBeforeHandlerValidation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	server := &Server{handler: http.NotFoundHandler()}
	ready := false
	err = server.ServeListenersWithLifecycle(
		context.Background(),
		func() error {
			ready = true
			return nil
		},
		nil,
		ListenerBinding{
			Listener: listener,
			Policy: ListenerPolicy{
				Kind:                  ListenerBrowser,
				RequireBrowserSession: true,
			},
		},
	)

	require.ErrorContains(t, err, "requires a web session manager")
	require.False(t, ready)
}
