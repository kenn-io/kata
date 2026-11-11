package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSocketActivationStoppedLifecycleGuards(t *testing.T) {
	for _, mode := range []string{"systemd", "launchd:api"} {
		for _, action := range []string{"start", "stop", "restart"} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				resetFlags(t)
				home := setupKataEnv(t)
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \""+mode+"\"\n"), 0600))
				old := startDetachedDaemon
				called := false
				startDetachedDaemon = func(context.Context, string, bool, bool) (daemonStartOutput, error) {
					called = true
					return daemonStartOutput{}, nil
				}
				t.Cleanup(func() { startDetachedDaemon = old })
				_, _, err := executeRootCapture(t, t.Context(), "daemon", action)
				require.Error(t, err)
				require.False(t, called, "manager-owned daemon must never be spawned")
			})
		}
	}
}

func TestSocketActivationConfigSaveRefusesBeforeWrite(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	path := filepath.Join(home, "config.toml")
	original := []byte("socket_activation = \"systemd\"\n")
	require.NoError(t, os.WriteFile(path, original, 0600))
	err := writeDaemonConfigAndRestart(t.Context(), new(bytes.Buffer), path, original, true, []byte("socket_activation = \"systemd\"\nlisten = \"127.0.0.1:7777\"\n"))
	require.Error(t, err)
	got, err := os.ReadFile(path) //nolint:gosec // G304: fixture config under the test-owned temporary home.
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestSocketActivationUpdateRequiresManager(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))
	err := restartDaemonAfterUpdate(t.Context(), new(bytes.Buffer), "", "v1.0.0")
	require.Error(t, err)
}

func TestSocketActivationRejectsBindConflictBeforePreparation(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\nlisten = \"127.0.0.1:7777\"\n"), 0600))
	_, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(home, "auth.token"))
	require.True(t, os.IsNotExist(err), "conflicting activation must not bootstrap credentials")
}

func TestSocketActivationAcquisitionNeverFallsBack(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"launchd:kata-test-unprovisioned-socket\"\n"), 0600))
	_, err := preflightDaemonStartup(t.Context(), "", false, true)
	require.Error(t, err)
}

func TestSocketActivationSystemdMissingNeverFallsBack(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))
	_, err := preflightDaemonStartup(t.Context(), "", false, true)
	require.Error(t, err)
}
