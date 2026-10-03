package client_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client"
)

// Selected auth-file failures stop every SDK transport before a request.
func TestNewWithGlobalAuthUnixRejectsBrokenSelectedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_AUTH_TOKEN_FILE", filepath.Join(home, "missing-token"))
	_, err := client.NewWithGlobalAuth(t.Context(), "unix:///tmp/example-daemon.sock")
	require.Error(t, err)
}
