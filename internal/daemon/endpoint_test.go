package daemon_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
)

func TestValidateNonPublicAddress(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:0",
		"10.0.0.1:0",
		"172.16.5.5:0",
		"192.168.1.1:0",
		"100.64.0.5:0",
		"169.254.1.1:0",
		"[fe80::1]:0",
		"[fc00::1]:0",
		"0.0.0.0:0",
		"[::]:0",
	} {
		assert.NoError(t, daemon.ValidateNonPublicAddress(addr), addr)
	}
}

func TestValidateNonPublicAddressRejectsPublicAndHostnames(t *testing.T) {
	for _, addr := range []string{
		"8.8.8.8:0",
		"[2001:4860:4860::8888]:0",
		"example.com:0",
		"localhost:0",
	} {
		require.Error(t, daemon.ValidateNonPublicAddress(addr), addr)
	}
}
