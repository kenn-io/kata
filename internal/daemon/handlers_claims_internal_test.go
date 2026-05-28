package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClaimHubClientHonorsTrustedPrivateNetwork(t *testing.T) {
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")

	client, err := newClaimHubClient("http://100.64.0.5:7787", "enrollment-token")

	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, "http://100.64.0.5:7787", client.baseURL)
}
