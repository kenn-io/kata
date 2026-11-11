//go:build !windows

package client_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/katatest"
)

func TestDiscoverManagerOwnedHomeNeverExecutesStartWith(t *testing.T) {
	home := isolateKataHome(t)
	server := katatest.New(t, katatest.WithUnixSocket())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0o600))
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	executable, invoked := fakeKataStarts(t, ns, server.Endpoint)
	_, err = client.Discover(t.Context(), client.DiscoverOptions{StartWith: executable})
	require.Error(t, err)
	assert.Empty(t, invoked(), "an explicit StartWith must not escape durable service-manager ownership")
}
