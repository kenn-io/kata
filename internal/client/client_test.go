package client

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalHTTPClientSelectsTransportByAddress(t *testing.T) {
	unixClient, unixBase := LocalHTTPClient("unix:///tmp/example.sock")
	require.Equal(t, UnixBase, unixBase)
	require.NotNil(t, unixClient.Transport, "unix addresses need the socket transport")

	tcpClient, tcpBase := LocalHTTPClient("127.0.0.1:7777")
	require.Equal(t, "http://127.0.0.1:7777", tcpBase)
	require.Nil(t, tcpClient.Transport)
}
