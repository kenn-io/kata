//go:build !windows

package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/pkg/client"
)

// TestDiscoverStartWithRealKata runs pkg/client's StartWith commands through
// the real kata CLI: a plain local entry starts, a pinned profile is recovered
// after its identity check, and a profile whose home has no storage is refused
// without creating any.
func TestDiscoverStartWithRealKata(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real daemons")
	}
	bin := buildKataBinary(t)
	home, workHome, lostHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TELEMETRY", "0")
	t.Chdir(t.TempDir())

	store, err := sqlitestore.Open(t.Context(), filepath.Join(workHome, "kata.db"))
	require.NoError(t, err)
	workUID := store.InstanceUID()
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), fmt.Appendf(nil,
		"[[daemon]]\nname = \"personal\"\nlocal = true\n\n"+
			"[[daemon]]\nname = \"work\"\nlocal = true\nhome = %q\ninstance_uid = %q\n\n"+
			"[[daemon]]\nname = \"lost\"\nlocal = true\nhome = %q\ninstance_uid = %q\n",
		workHome, workUID, lostHome, workUID), 0o600))
	for _, daemonHome := range []string{home, workHome} {
		t.Cleanup(func() {
			stop := exec.Command(bin, "daemon", "stop") //nolint:gosec // G204: bin is buildKataBinary's output
			stop.Env = append(os.Environ(), "KATA_HOME="+daemonHome)
			_ = stop.Run()
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	discover := func(name string) (*client.Client, error) {
		return client.Discover(ctx, client.DiscoverOptions{Daemon: name, StartWith: bin})
	}

	personal, err := discover("personal")
	require.NoError(t, err)
	ping, err := personal.PingWithResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, ping.StatusCode)

	work, err := discover("work")
	require.NoError(t, err)
	instance, err := work.InstanceWithResponse(ctx)
	require.NoError(t, err)
	require.NotNil(t, instance.JSON200)
	assert.Equal(t, workUID, instance.JSON200.InstanceUID)

	_, err = discover("lost")
	require.ErrorIs(t, err, client.ErrDaemonUnavailable)
	assert.Contains(t, err.Error(), "missing_profile_storage")
	entries, err := os.ReadDir(lostHome)
	require.NoError(t, err)
	assert.Empty(t, entries, "recovery must not create storage in a profile home")
}
