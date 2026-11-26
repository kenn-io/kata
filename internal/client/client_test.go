package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiscoveryClosesUnixConnections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unsupported on Windows")
	}
	dir, err := os.MkdirTemp("/tmp", "kata-sock-*")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	socket := filepath.Join(dir, "daemon.sock")
	listener, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	closed := make(chan struct{}, 1000)
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				closed <- struct{}{}
			}
		},
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	for range 1000 {
		_, _, err := probeAddressWithError(t.Context(), "unix://"+socket)
		require.NoError(t, err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range 1000 {
		select {
		case <-closed:
		case <-timer.C:
			t.Fatal("discovery connections remained open")
		}
	}
}

func TestUnixTransportClosesIdleConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := unixClient("unused", Opts{})
		transport := client.Transport.(*http.Transport)
		closed := make(chan error, 1)
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			clientConn, serverConn := net.Pipe()
			go func() {
				defer func() { _ = serverConn.Close() }()
				_, err := http.ReadRequest(bufio.NewReader(serverConn))
				if err == nil {
					_, err = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
				}
				if err == nil {
					_, err = io.Copy(io.Discard, serverConn)
				}
				closed <- err
			}()
			return clientConn, nil
		}
		defer client.CloseIdleConnections()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, UnixBase+"/", nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		select {
		case err := <-closed:
			require.NoError(t, err)
		case <-time.After(91 * time.Second):
			t.Fatal("idle connection remained open")
		}
	})
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

func TestLocalHTTPClientSelectsTCPAddress(t *testing.T) {
	tcpClient, tcpBase := LocalHTTPClient("127.0.0.1:7777")
	require.Equal(t, "http://127.0.0.1:7777", tcpBase)
	require.Nil(t, tcpClient.Transport)
}
