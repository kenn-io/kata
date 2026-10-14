package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayParentCredentialExpiration exercises relay parent credential expiration on the supplied native store.
// R3 links a narrow bridge to an ordinary account credential. A finite parent
// expiration must be representable without granting a forbidden issue subtree.
func RunRelayParentCredentialExpiration(t *testing.T, store db.Storage) {
	expires := time.Now().UTC().Add(time.Hour)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "expiring-parent-test-token", Actor: "member", AdminActor: "admin", ExpiresAt: &expires})
	require.NoError(t, err, "an ordinary bridge parent may have a bounded lifetime")
	require.Nil(t, token.Scope)
	require.NotNil(t, token.ExpiresAt)
	require.WithinDuration(t, expires, *token.ExpiresAt, time.Millisecond)
	resolved, err := store.ResolveAPIToken(t.Context(), "expiring-parent-test-token")
	require.NoError(t, err)
	require.Equal(t, token.ID, resolved.ID)
}
