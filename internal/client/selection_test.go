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
