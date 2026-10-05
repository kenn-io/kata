package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// R9: disconnect removes only the credential it observed, preserving newer
// reconnect/rebind credentials and making crash-after-delete retries harmless.
func TestFederationCredentialExactCleanup(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	store := config.DefaultFederationCredentialStore()
	cleanup, ok := store.(interface {
		DeleteFederationCredentialIfUnchanged(context.Context, string, config.FederationCredential) error
	})
	require.True(t, ok, "disconnect requires exact observed-credential cleanup")
	original := config.FederationCredential{HubURL: "https://hub.example", HubProjectID: 7, Token: "original-test-token", Actor: "member", SpokeProjectName: "shared-replica", LeavePending: true}
	changed := original
	changed.Token = "replacement-test-token"
	require.NoError(t, store.StoreFederationCredential(t.Context(), "project-test-uid", changed))
	require.ErrorIs(t, cleanup.DeleteFederationCredentialIfUnchanged(t.Context(), "project-test-uid", original), config.ErrFederationCredentialConflict)
	retained, found, err := store.FederationCredential(t.Context(), "project-test-uid")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, changed.Equal(retained))
	for range 2 {
		require.NoError(t, cleanup.DeleteFederationCredentialIfUnchanged(t.Context(), "project-test-uid", changed))
	}
	_, found, err = store.FederationCredential(t.Context(), "project-test-uid")
	require.NoError(t, err)
	require.False(t, found)
}
