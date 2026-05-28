package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefreshFederationBaselineAfterPurgeIgnoresCanceledRequestContext(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperHub(t, store, project.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := refreshFederationBaselineAfterPurge(ctx, ServerConfig{DB: store}, project.ID, "agent")

	require.NoError(t, err)
}
