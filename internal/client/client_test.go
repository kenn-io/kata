package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalDaemonUnreachableReason(t *testing.T) {
	failure := errors.New("probe failed")
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"network", fmt.Errorf("request: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:1234", Err: &net.OpError{Op: "dial", Net: "tcp", Err: failure}})},
		{"url", fmt.Errorf("request: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:1234", Err: failure})},
		{"plain", failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := &LocalDaemonUnreachableError{PID: 42, Address: "127.0.0.1:1234", DataDir: dir, cause: tc.cause}
			logs := fmt.Sprintf("Check for logs at %q if it exists", filepath.Join(dir, "daemon.log"))
			require.Equal(t, "probe failed. "+logs, err.Reason())
			require.Equal(t, fmt.Sprintf("daemon pid 42 is running at 127.0.0.1:1234 but is unreachable: %v. %s", tc.cause, logs), err.Summary())
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
