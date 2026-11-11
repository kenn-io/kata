package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSocketActivationProfileNeverSpawns(t *testing.T) {
	profile, root := localProfileFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))
	state := patchEnsureHooks(t, "new-build", "http://replacement-daemon")
	_, err := EnsureResolvedInWorkspace(t.Context(), root)
	require.Error(t, err)
	require.Zero(t, state.stopCalls)
	require.Zero(t, state.startCalls)
}
