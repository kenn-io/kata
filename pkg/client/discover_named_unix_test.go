//go:build !windows

package client_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/katatest"
)

func TestDiscoverNamedUnixCredentialsAndPrecedence(t *testing.T) {
	for _, source := range []string{"named", "active"} {
		t.Run(source, func(t *testing.T) {
			home := isolateKataHome(t)
			t.Setenv("KATA_DSN", filepath.Join(home, "kata.db"))
			a := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("token-a"))
			b := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("token-b"))
			initProject(t, a.Client(t), "other-project")
			fileA, fileB := filepath.Join(home, "token-a"), filepath.Join(home, "token-b")
			require.NoError(t, os.WriteFile(fileA, []byte("token-a\n"), 0o600))
			require.NoError(t, os.WriteFile(fileB, []byte("token-b\n"), 0o600))
			t.Setenv("KATA_AUTH_TOKEN_FILE", fileA)
			catalog := fmt.Sprintf("active_daemon = \"selected\"\n[[daemon]]\nname = \"other\"\nurl = %q\ntoken_file = %q\n[[daemon]]\nname = \"selected\"\nurl = %q\ntoken_file = %q\n", a.Endpoint, fileA, b.Endpoint, fileB)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(catalog), 0o600))
			options := client.DiscoverOptions{StartWith: filepath.Join(home, "must-not-execute")}
			if source == "named" {
				options.Daemon = "selected"
				t.Setenv("KATA_SERVER", a.Endpoint)
			}
			api, err := client.Discover(t.Context(), options)
			require.NoError(t, err)
			initProject(t, api, "selected-project")
			assert.Equal(t, "selected-project", listProjects(t, b.Client(t))[0].Name)
			assert.Equal(t, "other-project", listProjects(t, a.Client(t))[0].Name)
			require.NoError(t, os.Remove(fileB))
			_, err = client.Discover(t.Context(), options)
			require.Error(t, err)
			assert.Contains(t, err.Error(), fileB)
			// Preserve the existing deliberate inline-token override, but not an
			// unrelated KATA_AUTH_TOKEN_FILE override of a named credential source.
			t.Setenv("KATA_AUTH_TOKEN", "token-b")
			override, err := client.Discover(t.Context(), options)
			require.NoError(t, err)
			assert.Equal(t, "selected-project", listProjects(t, override)[0].Name)
		})
	}
}
