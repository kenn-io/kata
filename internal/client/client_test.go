package client

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiscoveryClosesUnixConnections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unsupported on Windows")
	}
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	var open atomic.Int64
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				open.Add(1)
			case http.StateClosed, http.StateHijacked:
				open.Add(-1)
			}
		},
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	for _, probe := range []struct {
		name string
		call func() error
	}{
		{"discovery", func() error { _, _, err := probeAddressWithError(t.Context(), "unix://"+socket); return err }},
		{"diagnostics", func() error { _, _, err := probeAddressReadOnly(t.Context(), "unix://"+socket); return err }},
	} {
		t.Run(probe.name, func(t *testing.T) {
			for range 1000 {
				require.NoError(t, probe.call())
			}
			require.Eventually(t, func() bool { return open.Load() == 0 }, time.Second, 10*time.Millisecond)
		})
	}
}

func TestLocalDaemonUnreachableReason(t *testing.T) {
	failure := errors.New("probe failed")
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"network", fmt.Errorf("request: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:1234", Err: &net.OpError{Op: "dial", Net: "tcp", Err: failure}})},
		{"url", fmt.Errorf("request: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:1234", Err: failure})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := &LocalDaemonUnreachableError{PID: 42, Address: "127.0.0.1:1234", DataDir: dir, cause: tc.cause}
			logs := fmt.Sprintf("Check for logs at %q if it exists", filepath.Join(dir, "daemon.log"))
			require.Equal(t, "probe failed. "+logs, err.Reason())
		})
	}
}

func TestLocalHTTPClientSelectsTransportByAddress(t *testing.T) {
	unixClient, unixBase := LocalHTTPClient("unix:///tmp/example.sock")
	require.Equal(t, UnixBase, unixBase)
	require.NotNil(t, unixClient.Transport, "unix addresses need the socket transport")

	tcpClient, tcpBase := LocalHTTPClient("127.0.0.1:7777")
	require.Equal(t, "http://127.0.0.1:7777", tcpBase)
	require.Nil(t, tcpClient.Transport)
}
