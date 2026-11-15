package client

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestEnsureSocketActivationNeverSpawnsOrReplaces(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
			stageStorageSchema(t, filepath.Join(home, "kata.db"), db.CurrentSchemaVersion())
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))
			if live {
				_, addr := startMockDaemonPing(t, map[string]any{"ok": true, "service": "kata", "version": "old-build", "pid": os.Getpid()})
				require.NoError(t, writeRuntimeRecord(t, home, addr))
			}
			state := patchEnsureHooks(t, "new-build", "http://replacement-daemon")
			_, err := EnsureLocalRunningTarget(t.Context())
			require.Error(t, err)
			require.Zero(t, state.stopCalls, "manager-owned daemon must not be signaled")
			require.Zero(t, state.startCalls, "manager-owned daemon must not be spawned")
		})
	}
}
