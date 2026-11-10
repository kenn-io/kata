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
	fileSetup := func(t *testing.T, path string) string {
		t.Helper()
		f, err := safefileio.CreatePrivateFile(path)
		require.NoError(t, err)
		_, err = f.Write([]byte("first-hub-token\n"))
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return fmt.Sprintf("token_file=%q\n", path)
	}
	for _, tc := range []struct {
		name          string
		setup         func(*testing.T, string) string
		mutate        func(*testing.T, string)
		want, wantErr string
	}{
		{name: "remove", setup: fileSetup, mutate: func(t *testing.T, path string) {
			t.Helper()
			require.NoError(t, os.Remove(path))
		}, wantErr: "file"},
		{name: "rotate", setup: fileSetup, mutate: func(t *testing.T, path string) {
			t.Helper()
			require.NoError(t, os.WriteFile(path, []byte("second-hub-token\n"), 0o600))
		}, want: "second-hub-token"},
		{name: "env cleared", setup: func(t *testing.T, _ string) string {
			t.Helper()
			t.Setenv("EXAMPLE_HUB_TOKEN", "first-hub-token")
			return "token_env='EXAMPLE_HUB_TOKEN'\n"
		}, mutate: func(t *testing.T, _ string) {
			t.Helper()
			t.Setenv("EXAMPLE_HUB_TOKEN", "")
		}, wantErr: "env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("KATA_AUTH_TOKEN", "local-only-token")
			path := filepath.Join(home, "hub-token")
			fields := tc.setup(t, path)
			srv := startTUIPingServer(t)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname='hub'\nurl=%q\n%s", srv.URL, fields)), 0o600))
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err)
			// Browsing saves this resolved target for enrollment confirmation.
			_, browsed, err := newHubAdminClient(t.Context(), daemonTargetsFromConfig(cfg.Daemons)[0])
			require.NoError(t, err)
			require.Equal(t, "first-hub-token", browsed.resolved.Token)
			tc.mutate(t, path)
			_, next, err := newHubAdminClient(t.Context(), browsed)
			switch tc.wantErr {
			case "file":
				require.ErrorIs(t, err, config.ErrCredentialSource)
			case "env":
				var envErr *config.TokenEnvError
				require.ErrorAs(t, err, &envErr)
			default:
				require.NoError(t, err)
				require.Equal(t, tc.want, next.resolved.Token)
			}
		})
	}
}
