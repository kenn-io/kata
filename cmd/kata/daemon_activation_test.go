package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
)

func requireActivationManagerRefusal(t *testing.T, err error, mode string) {
	t.Helper()
	manager, _, _ := strings.Cut(mode, ":")
	require.ErrorContains(t, err, "socket_activation=")
	require.ErrorContains(t, err, mode)
	require.ErrorContains(t, err, "use "+manager)
	require.ErrorContains(t, err, "not kata daemon start/stop/restart")
}

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
				requireActivationManagerRefusal(t, err, mode)
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
	requireActivationManagerRefusal(t, err, "systemd")
	got, err := os.ReadFile(path) //nolint:gosec // G304: fixture config under the test-owned temporary home.
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestSocketActivationUpdateRequiresManager(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))
	err := restartDaemonAfterUpdate(t.Context(), new(bytes.Buffer), "", "v1.0.0")
	requireActivationManagerRefusal(t, err, "systemd")
}

func TestSocketActivationRejectsBindConflictBeforePreparation(t *testing.T) {
	for _, source := range []string{"cli", "config", "environment", "port"} {
		t.Run(source, func(t *testing.T) {
			resetFlags(t)
			home := setupKataEnv(t)
			for _, name := range []string{"KATA_DSN", "KATA_AUTH_TOKEN", "KATA_AUTH_TOKEN_FILE", "KATA_AUTOSTART", "KATA_LISTEN", "KATA_WEB_LISTEN", "KATA_WEB_PUBLIC_ORIGIN", "KATA_MCP_ENABLED", "KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES", "PORT", daemonruntime.AutoStartMarkerEnv} {
				t.Setenv(name, "")
			}
			t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")
			var listen, configBody string
			conflict := "socket_activation conflicts with effective listen configuration"
			switch source {
			case "cli":
				listen = "100.64.0.5:7777"
			case "config":
				configBody = "listen = \"100.64.0.5:7777\"\n"
			case "environment":
				t.Setenv("KATA_LISTEN", "100.64.0.5:7777")
			case "port":
				t.Setenv("PORT", "7777")
				conflict = "socket_activation conflicts with eligible PORT listen configuration"
			}
			path := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("socket_activation = \"systemd\"\n"+configBody), 0600))
			_, err := preflightDaemonStartup(t.Context(), listen, false, false)
			require.ErrorContains(t, err, conflict)
			tokenPath := filepath.Join(home, "auth-token")
			_, err = os.Stat(tokenPath)
			require.ErrorIs(t, err, os.ErrNotExist, "conflicting activation must not bootstrap credentials")

			// With only manager ownership removed, the same exposed listener
			// must reach auth preparation and mint a persisted credential.
			require.NoError(t, os.WriteFile(path, []byte(configBody), 0600))
			startup, err := preflightDaemonStartup(t.Context(), listen, false, false)
			require.NoError(t, err)
			token, err := os.ReadFile(tokenPath) //nolint:gosec // G304: test-owned credential path.
			require.NoError(t, err)
			require.NotEmpty(t, strings.TrimSpace(string(token)))
			require.Equal(t, strings.TrimSpace(string(token)), startup.Config.Auth.Token)
		})
	}
}

func TestSocketActivationAutoStartIgnoresPORTAndRequiresManager(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("KATA_LISTEN", "")
	t.Setenv("PORT", "7777")
	t.Setenv(daemonruntime.AutoStartMarkerEnv, "1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))
	_, err := preflightDaemonStartup(t.Context(), "", false, false)
	requireActivationManagerRefusal(t, err, "systemd")
}
