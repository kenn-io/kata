//go:build linux || darwin

package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
)

func activationSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "activation-listener-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "api.sock")
}

func activationListenerFixture(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	socket := activationSocketPath(t)
	listener, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return listener.(*net.UnixListener), socket
}

// Darwin does not implement SO_ACCEPTCONN getsockopt. A passive launchd-style
// descriptor must still reach serving, while connected descriptors stay invalid.
func TestActivatedFilesServeWithoutTakingManagerOwnership(t *testing.T) {
	manager, socket := activationListenerFixture(t)
	before, err := os.Stat(socket)
	require.NoError(t, err)
	file, err := manager.File()
	require.NoError(t, err)
	defer file.Close()
	adopted, err := adoptActivatedFiles(t.Context(), []*os.File{file})
	require.NoError(t, err)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "activated\n") })}
	go func() { _ = server.Serve(adopted) }()
	t.Cleanup(func() { _ = server.Close() })
	hc, base := clientpkg.LocalHTTPClient("unix://" + socket)
	t.Cleanup(hc.CloseIdleConnections)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base, nil)
	require.NoError(t, err)
	response, err := hc.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "activated\n", string(body))
	require.NoError(t, server.Close())
	after, err := os.Stat(socket)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
}

func TestActivatedFilesRejectAcceptedConnection(t *testing.T) {
	manager, socket := activationListenerFixture(t)
	connection, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
	require.NoError(t, err)
	defer connection.Close()
	accepted, err := manager.AcceptUnix()
	require.NoError(t, err)
	defer accepted.Close()
	file, err := accepted.File()
	require.NoError(t, err)
	defer file.Close()
	_, err = adoptActivatedFiles(t.Context(), []*os.File{file})
	require.Error(t, err, "an accepted connection is not a server listener")
}
