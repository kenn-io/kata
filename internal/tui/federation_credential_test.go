package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/safefileio"
)

func TestTUIHubFileIsReadAgainAfterBrowse(t *testing.T) {
	for _, action := range []string{"remove", "rotate"} {
		t.Run(action, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("KATA_AUTH_TOKEN", "local-only-token")
			path := filepath.Join(home, "hub-token")
			f, err := safefileio.CreatePrivateFile(path)
			require.NoError(t, err)
			_, err = f.Write([]byte("first-hub-token\n"))
			require.NoError(t, err)
			require.NoError(t, f.Close())
			srv := startTUIPingServer(t)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname='hub'\nurl=%q\ntoken_file=%q\n", srv.URL, path)), 0o600))
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err)
			// fetchFederationHubProjects returns this resolved target, which
			// handleFederationHubProjectsLoaded stores in draft.HubTarget.
			_, browsed, err := newHubAdminClient(t.Context(), daemonTargetsFromConfig(cfg.Daemons)[0])
			require.NoError(t, err)
			require.Equal(t, "first-hub-token", browsed.resolved.Token)
			if action == "remove" {
				require.NoError(t, os.Remove(path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte("second-hub-token\n"), 0o600))
			}
			// runFederationEnrollment calls this again with draft.HubTarget.
			_, next, err := newHubAdminClient(t.Context(), browsed)
			if action == "remove" {
				require.ErrorIs(t, err, config.ErrCredentialSource, "enrollment must stop when selected file disappears")
			} else {
				require.NoError(t, err)
				require.Equal(t, "second-hub-token", next.resolved.Token, "enrollment must use the current selected file")
			}
		})
	}
}

func TestTUIHubEnvIsReadAgainAfterBrowse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "local-only-token")
	t.Setenv("EXAMPLE_HUB_TOKEN", "first-hub-token")
	srv := startTUIPingServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname='hub'\nurl=%q\ntoken_env='EXAMPLE_HUB_TOKEN'\n", srv.URL)), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	_, browsed, err := newHubAdminClient(t.Context(), daemonTargetsFromConfig(cfg.Daemons)[0])
	require.NoError(t, err)
	t.Setenv("EXAMPLE_HUB_TOKEN", "")
	_, _, err = newHubAdminClient(t.Context(), browsed)
	var envErr *config.TokenEnvError
	require.ErrorAs(t, err, &envErr, "existing env selection must still be read after browse")
}

func TestTUIHubInlineCredentialWinsAfterBrowse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "local-only-token")
	t.Setenv("EXAMPLE_HUB_TOKEN", "env-token")
	srv := startTUIPingServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname='hub'\nurl=%q\ntoken='inline-hub-token'\ntoken_file='/missing/token'\ntoken_env='EXAMPLE_HUB_TOKEN'\n", srv.URL)), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	_, browsed, err := newHubAdminClient(t.Context(), daemonTargetsFromConfig(cfg.Daemons)[0])
	require.NoError(t, err)
	_, next, err := newHubAdminClient(t.Context(), browsed)
	require.NoError(t, err)
	require.Equal(t, "inline-hub-token", next.resolved.Token)
}
