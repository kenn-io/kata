package client

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

// An active local catalog entry reports an unset or empty token_env the way
// every other catalog entry does: by daemon and variable, so the user knows
// what to export.
func TestActiveLocalCatalogTokenEnvErrorNamesVariable(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("EXAMPLE_LOCAL_TOKEN", "")
	require.NoError(t, writeRawConfig(home, `active_daemon = "personal"

[[daemon]]
name = "personal"
local = true
token_env = "EXAMPLE_LOCAL_TOKEN"
`))
	workspace := t.TempDir()
	writeNeutralWorkspaceMarker(t, workspace)

	_, err := EnsureResolvedInWorkspaceForTUI(t.Context(), workspace)
	tokenErr, ok := errors.AsType[*config.TokenEnvError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, config.TokenEnvError{Daemon: "personal", Env: "EXAMPLE_LOCAL_TOKEN"}, *tokenErr)
}

// Selection stays credential-free, as `kata daemon locate` requires, but
// every path that resolves the selected daemon reports a catalog entry whose
// token_env variable is unset or empty. KATA_AUTH_TOKEN replaces only a remote
// entry's credential.
func TestCatalogCredentialIsReportedByResolutionNotSelection(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("EXAMPLE_HUB_TOKEN", "")
	t.Setenv("EXAMPLE_LOCAL_TOKEN", "")
	require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "hub"
url = "https://daemon.example"
token_env = "EXAMPLE_HUB_TOKEN"

[[daemon]]
name = "personal"
local = true
token_env = "EXAMPLE_LOCAL_TOKEN"
`))

	for name, env := range map[string]string{"hub": "EXAMPLE_HUB_TOKEN", "personal": "EXAMPLE_LOCAL_TOKEN"} {
		want := `daemon "` + name + `": token_env "` + env + `" is unset or empty`
		_, err := InspectSelection(t.Context(), "", name)
		require.NoError(t, err, "selection reads no credential for %s", name)
		_, _, err = DiscoverSelected(t.Context(), DiscoverRequest{Name: name})
		require.EqualError(t, err, want)
		_, err = PrepareResolvedNamed(t.Context(), name)
		require.EqualError(t, err, want)
	}

	t.Setenv("KATA_AUTH_TOKEN", "override-token")
	resolved, err := PrepareResolvedNamed(t.Context(), "hub")
	require.NoError(t, err, "KATA_AUTH_TOKEN replaces a remote entry's credential")
	assert.Equal(t, "override-token", resolved.Token)
	_, err = PrepareResolvedNamed(t.Context(), "personal")
	require.Error(t, err, "KATA_AUTH_TOKEN does not replace a local entry's credential")
}
