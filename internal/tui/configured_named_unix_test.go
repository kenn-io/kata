package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
)

func TestConfiguredUnixTUINamedSwitchPreservesSocketAndCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_AUTH_TOKEN_FILE", "")
	a, _, aCalls := tuiUnixBackend(t, "named-a", "token-a")
	b, _, _ := tuiUnixBackend(t, "named-b", "token-b")
	tokenA, tokenB := filepath.Join(home, "a.token"), filepath.Join(home, "b.token")
	require.NoError(t, os.WriteFile(tokenA, []byte("token-a"), 0600))
	require.NoError(t, os.WriteFile(tokenB, []byte("token-b"), 0600))
	catalog := fmt.Sprintf("[[daemon]]\nname=\"a\"\nurl=%q\ntoken_file=%q\n[[daemon]]\nname=\"b\"\nurl=%q\ntoken_file=%q\n", a.Address, tokenA, b.Address, tokenB)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(catalog), 0600))
	// Resolve through the shared named selector, but avoid startup probing:
	// connectivity and credentials are exercised by the actual API requests.
	oldEnsure := ensureResolvedNamedForTUI
	t.Cleanup(func() { ensureResolvedNamedForTUI = oldEnsure })
	ensureResolvedNamedForTUI = clientpkg.PrepareResolvedNamed
	first, err := connectDaemonTarget(t.Context(), daemonTarget{Name: "a", URL: a.Address, skipInitialScope: true})
	require.NoError(t, err)
	instance, err := first.api.GetInstance(t.Context())
	require.NoError(t, err)
	require.Equal(t, "named-a", instance.InstanceUID)
	beforeSwitch := aCalls.Load()
	second, err := connectDaemonTarget(t.Context(), daemonTarget{Name: "b", URL: b.Address, skipInitialScope: true})
	require.NoError(t, err)
	instance, err = second.api.GetInstance(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "named-b", instance.InstanceUID)
	assert.Equal(t, beforeSwitch, aCalls.Load())
	assert.Equal(t, b.Address, daemonEndpoint(second.target))
	rows := daemonRows([]daemonTarget{{URL: a.Address}, {URL: b.Address}}, second.target)
	require.Len(t, rows, 2)
	assert.False(t, rows[0].current)
	assert.True(t, rows[1].current)
	require.NoError(t, os.Remove(tokenB))
	_, err = connectDaemonTarget(t.Context(), daemonTarget{Name: "b", URL: b.Address, skipInitialScope: true})
	require.Error(t, err, "broken selected credentials must not fall back to another target")
	assert.Equal(t, beforeSwitch, aCalls.Load())
}
