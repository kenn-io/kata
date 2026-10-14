package dbtest

import (
	"crypto/ed25519"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayAckTouchesOnlyUnacknowledgedRows proves that acknowledging a batch
// updates only the rows above the binding's previous acknowledged cursor. The
// backend counter observes every outbox row an acknowledgement UPDATE matches,
// so an acknowledgement that rewrote the whole historical prefix is visible as
// a count above the number of deliveries.
func RunRelayAckTouchesOnlyUnacknowledgedRows(t *testing.T, store db.Storage, backend Backend) {
	require.NotNil(t, backend.InstallRelayAckRowCounter)
	const deliveries, batch = 1000, 100
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "ack-bound-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "ack-bound-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "ack-bound-relay-test-token"})
	require.NoError(t, err)
	binding := grant.Enrollment.RelayBindingUID
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "member")
	for i := range deliveries {
		_, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("Delivery %d", i), Author: "assistant"})
		require.NoError(t, err)
	}

	count, remove, err := backend.InstallRelayAckRowCounter(ctx, store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remove()) })
	acked := 0
	var first, last db.RelayEnvelope
	for {
		pending, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, batch)
		require.NoError(t, err)
		if len(pending) == 0 {
			break
		}
		require.Len(t, pending, batch, "every batch is full until the queue drains")
		if acked == 0 {
			first = pending[len(pending)-1]
		}
		last = pending[len(pending)-1]
		require.NoError(t, store.AckRelayDeliveries(ctx, binding, last.Epoch, db.RelayStreamEvent, last.Sequence, last.Digest))
		acked += len(pending)
	}
	require.Equal(t, deliveries, acked)
	updated, err := count(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(deliveries), updated, "acknowledging %d deliveries in batches of %d must update each outbox row once; rewriting the acknowledged prefix on every batch updates %d rows", deliveries, batch, batch*(deliveries/batch)*(deliveries/batch+1)/2)

	require.NoError(t, store.AckRelayDeliveries(ctx, binding, last.Epoch, db.RelayStreamEvent, last.Sequence, last.Digest), "a lost final reply is retried")
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, first.Epoch, db.RelayStreamEvent, first.Sequence, first.Digest), "a lost earlier reply is retried")
	retried, err := count(ctx)
	require.NoError(t, err)
	require.Equal(t, updated, retried, "a retried acknowledgement below the acknowledged cursor matches no rows")
	require.Error(t, store.AckRelayDeliveries(ctx, binding, last.Epoch, db.RelayStreamEvent, last.Sequence, "wrong-digest"), "a retried acknowledgement still names the exact emitted envelope")

	_, _, err = store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "After the drain", Author: "assistant"})
	require.NoError(t, err)
	next, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, batch)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, next[0].Epoch, db.RelayStreamEvent, next[0].Sequence, next[0].Digest))
	afterDrain, err := count(ctx)
	require.NoError(t, err)
	require.Equal(t, updated+1, afterDrain, "a delivery queued above the cursor is acknowledged once")

	var cursor *db.RelayCursorExport
	for record, err := range store.ExportRelayState(ctx) {
		require.NoError(t, err)
		switch value := record.(type) {
		case *db.RelayOutboxExport:
			if value.BindingUID == binding && value.Stream == db.RelayStreamEvent {
				require.True(t, value.Acknowledged, "outbox row %d stays acknowledged", value.ID)
			}
		case *db.RelayCursorExport:
			if value.BindingUID == binding && value.Stream == db.RelayStreamEvent {
				cursor = value
			}
		}
	}
	require.NotNil(t, cursor)
	require.Equal(t, next[0].Sequence, cursor.AcknowledgedThrough)
}
