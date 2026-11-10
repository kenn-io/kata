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
	for _, tc := range []struct {
		name, fields, first, want string
	}{
		{"remove", "token_file=%q\n", "first-hub-token", ""},
		{"rotate", "token_file=%q\n", "first-hub-token", "second-hub-token"},
		{"env cleared", "token_env='EXAMPLE_HUB_TOKEN'\n", "first-hub-token", ""},
		{"inline wins", "token='inline-hub-token'\ntoken_file=%q\ntoken_env='EXAMPLE_HUB_TOKEN'\n", "inline-hub-token", "inline-hub-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("KATA_AUTH_TOKEN", "local-only-token")
			t.Setenv("EXAMPLE_HUB_TOKEN", "first-hub-token")
			path := filepath.Join(home, "hub-token")
			fields := tc.fields
			if tc.name != "env cleared" {
				fields = fmt.Sprintf(fields, path)
			}
			if tc.name == "remove" || tc.name == "rotate" {
				f, err := safefileio.CreatePrivateFile(path)
				require.NoError(t, err)
				_, err = f.Write([]byte("first-hub-token\n"))
				require.NoError(t, err)
				require.NoError(t, f.Close())
			}
			srv := startTUIPingServer(t)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname='hub'\nurl=%q\n%s", srv.URL, fields)), 0o600))
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err)
			// Browsing saves this resolved target for enrollment confirmation.
			_, browsed, err := newHubAdminClient(t.Context(), daemonTargetsFromConfig(cfg.Daemons)[0])
			require.NoError(t, err)
			require.Equal(t, tc.first, browsed.resolved.Token)
			switch tc.name {
			case "remove":
				require.NoError(t, os.Remove(path))
			case "rotate":
				require.NoError(t, os.WriteFile(path, []byte("second-hub-token\n"), 0o600))
			case "env cleared":
				t.Setenv("EXAMPLE_HUB_TOKEN", "")
			}
			_, next, err := newHubAdminClient(t.Context(), browsed)
			switch tc.name {
			case "remove":
				require.ErrorIs(t, err, config.ErrCredentialSource)
			case "env cleared":
				var envErr *config.TokenEnvError
				require.ErrorAs(t, err, &envErr)
			default:
				require.NoError(t, err)
				require.Equal(t, tc.want, next.resolved.Token)
			}
		})
	}
}
