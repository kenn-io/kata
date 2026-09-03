//go:build !windows

package main

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectionDroppedErrnosUnix(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPIPE, syscall.ECONNRESET} {
		err := &url.Error{Op: http.MethodPost, URL: "https://daemon.example/issues", Err: &net.OpError{
			Op: "write", Net: "tcp", Err: errno,
		}}
		got := createRequestError(err, false)
		var cliErr *cliError
		require.ErrorAs(t, got, &cliErr)
		assert.Equal(t, "create_outcome_unknown", cliErr.Code)
		assert.NotContains(t, cliErr.Message, "daemon.example")
	}

	refused := &url.Error{Op: http.MethodPost, URL: "https://daemon.example/issues", Err: &net.OpError{
		Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
	}}
	assert.Same(t, refused, createRequestError(refused, false))
}
