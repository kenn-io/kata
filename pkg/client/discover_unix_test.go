//go:build !windows

package client_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

// Explicit targets identify the transport before any runtime record exists.
// They must neither use the default database nor invoke StartWith if unavailable.
func TestDiscoverExplicitUnixSelection(t *testing.T) {
	for _, source := range []string{"environment", "workspace"} {
		t.Run(source, func(t *testing.T) {
			home := isolateKataHome(t)
			t.Setenv("KATA_DSN", filepath.Join(home, "kata.db"))
			t.Setenv("KATA_AUTH_TOKEN_FILE", "")
			local := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("default-token"))
			initProject(t, local.Client(t), "default-project")
			publishLocalRuntime(t, home, local.Endpoint)
			selected := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("selected-token"))
			tokenFile := filepath.Join(home, "client-token")
			require.NoError(t, os.WriteFile(tokenFile, []byte("selected-token\n"), 0o600))
			t.Setenv("KATA_AUTH_TOKEN_FILE", tokenFile)
			workspace := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version = 1\n[project]\nname = \"selected-project\"\n"), 0o600))
			if source == "environment" {
				t.Setenv("KATA_SERVER", selected.Endpoint)
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte(fmt.Sprintf("version = 1\n[server]\nurl = %q\n", selected.Endpoint)), 0o600))
			}
			options := client.DiscoverOptions{Workspace: workspace, StartWith: filepath.Join(home, "must-not-execute")}
			api, err := client.Discover(t.Context(), options)
			require.NoError(t, err)
			discovered, ok := api.Discovered()
			require.True(t, ok)
			assert.True(t, discovered.Remote, "an explicit socket does not grant current-home filesystem assumptions")
			initProject(t, api, "selected-project")
			projects := listProjects(t, selected.Client(t))
			require.Len(t, projects, 1)
			assert.Equal(t, "selected-project", projects[0].Name)
			defaultProjects := listProjects(t, local.Client(t))
			require.Len(t, defaultProjects, 1)
			assert.Equal(t, "default-project", defaultProjects[0].Name)
			require.NoError(t, os.Remove(strings.TrimPrefix(selected.Endpoint, "unix://")))
			unavailable, err := client.Discover(t.Context(), options)
			require.NoError(t, err, "configured endpoints do not require runtime discovery or StartWith")
			_, err = unavailable.ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), strings.TrimPrefix(selected.Endpoint, "unix://"))
			assert.Equal(t, "default-project", listProjects(t, local.Client(t))[0].Name)
		})
	}
}

func TestDiscoverExplicitUnixCredentialFailure(t *testing.T) {
	home := isolateKataHome(t)
	selected := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("selected-token"))
	t.Setenv("KATA_SERVER", selected.Endpoint)
	t.Setenv("KATA_AUTH_TOKEN_FILE", filepath.Join(home, "missing-token"))
	_, err := client.Discover(t.Context(), client.DiscoverOptions{StartWith: filepath.Join(home, "must-not-execute")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing-token")
}
