package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Contract: credentials are daemon-owned selectors; bindings never carry tokens.
func TestNormalizeLinearSyncConfig(t *testing.T) {
	c, err := NormalizeLinearSyncConfig(LinearSyncConfig{})
	require.NoError(t, err)
	require.Equal(t, "KATA_LINEAR_TOKEN", c.TokenEnv)
	require.Equal(t, "api-key", c.AuthType)
	c, err = NormalizeLinearSyncConfig(LinearSyncConfig{TokenEnv: " EXAMPLE_LINEAR_TOKEN ", AuthType: "oauth"})
	require.NoError(t, err)
	require.Equal(t, "EXAMPLE_LINEAR_TOKEN", c.TokenEnv)
	for _, c := range []LinearSyncConfig{{TokenEnv: "bad-key"}, {AuthType: "guess"}} {
		_, err := NormalizeLinearSyncConfig(c)
		require.Error(t, err)
	}
}
