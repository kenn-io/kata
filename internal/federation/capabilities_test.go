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
