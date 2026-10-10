package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/federation"
)

func TestNormalizeCapabilitiesMapsLeaseToClaim(t *testing.T) {
	got, err := federation.NormalizeCapabilities("pull,push,lease")
	require.NoError(t, err)
	assert.Equal(t, "claim,pull,push", got.API)
	assert.Equal(t, "pull,push,lease", got.Display)
}

func TestNormalizeCapabilitiesAcceptsClaimAndDisplaysLease(t *testing.T) {
	got, err := federation.NormalizeCapabilities("claim,pull,push")
	require.NoError(t, err)
	assert.Equal(t, "claim,pull,push", got.API)
	assert.Equal(t, "pull,push,lease", got.Display)
}

func TestNormalizeCapabilitiesRejectsUnknown(t *testing.T) {
	_, err := federation.NormalizeCapabilities("pull,admin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown federation capability "admin"`)
}

func TestNormalizeCapabilitiesRoundTripsDisplayBundle(t *testing.T) {
	requested, err := federation.NormalizeCapabilities("pull,push,lease")
	require.NoError(t, err)
	require.Equal(t, "claim,pull,push", requested.API)
	require.Equal(t, "pull,push,lease", requested.Display)
	joined, err := federation.NormalizeCapabilities(requested.Display)
	require.NoError(t, err)
	require.Equal(t, requested.API, joined.API, "the displayed enrollment bundle is also the join capability input")
	defaults, err := federation.NormalizeCapabilities("")
	require.NoError(t, err)
	require.Equal(t, "claim,pull,push", defaults.API)
	require.Equal(t, "pull,push,lease", defaults.Display)
}
