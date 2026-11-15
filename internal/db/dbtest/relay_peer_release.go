package dbtest

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayPeerRelease checks whole-response refusal and retained retries.
func RunRelayPeerRelease(t *testing.T, store db.Storage) {
	fixture := NewRelaySharedAccessFixture(t, store)
	binding := fixture.Grant.Enrollment.RelayBindingUID
	ctx := db.WithRelayPeerEventFeatures(t.Context(), db.SupportedEventFeatures)
	denied := errors.New("peer denied")
	rejecting := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	before := relayStateForBinding(t, store, binding)
	out, err := store.PendingRelayDeliveries(rejecting, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
	offered, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, offered)
	out, err = store.PendingRelayDeliveries(rejecting, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	retried, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, retried)

	reset := store.(db.RelayResetStore)
	before = relayStateForBinding(t, store, binding)
	grants, err := store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	maxEvent, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	blocked, err := reset.CreateRelayReset(rejecting, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
	afterGrants, err := store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	require.Equal(t, grants, afterGrants)
	currentEvent, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, maxEvent, currentEvent)
	checkpoint, err := reset.CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
	public := fixture.Signer.PrivateKey.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: fixture.Projects[0].UID, AuthorityUID: fixture.Signer.AuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	before = relayStateForBinding(t, store, binding)
	blocked, err = reset.CreateRelayReset(rejecting, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
	repeated, err := reset.CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, repeated)
}
