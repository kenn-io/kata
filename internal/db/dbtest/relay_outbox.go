package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayOutboxAtomicity exercises relay outbox atomicity on the supplied native store.
// R4: queue intent commits with source data, survives retries, and acknowledges
// only an actually emitted prefix in its binding/stream/reset namespace.
func RunRelayOutboxAtomicity(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "outbox-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "outbox-relay-test-token"})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "member")
	issue, creation, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Queued with source", Author: "assistant"})
	require.NoError(t, err)
	comment, commentEvent, err := store.CreateComment(writeCtx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-assistant", Body: "Queued comment", Teammate: "reviewer"})
	require.NoError(t, err)
	_ = comment
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	featureEvent := argvCompatRemoteWorkflow(t, project, store.InstanceUID(), `{"steps":[{"kind":"argv","argv":["true"]}]}`)
	insertFeature := func() {
		_, err := q.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, featureEvent.EventUID, store.InstanceUID(), project.ID, project.Name, featureEvent.Type, "worker", string(featureEvent.Payload), featureEvent.HLCPhysicalMS, featureEvent.HLCCounter, featureEvent.ContentHash)
		require.NoError(t, err)
	}
	removeFeature := func() {
		_, err := q.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, featureEvent.EventUID)
		require.NoError(t, err)
	}
	peerCtx := db.WithRelayPeerFeatures(ctx, grant.Enrollment.RelayBindingUID, "")
	insertFeature()
	_, err = store.PendingRelayDeliveries(peerCtx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	_, err = store.AcceptRelayDeliveries(peerCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	var emittedCursor, acceptedCursor int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COALESCE(MAX(emitted_through),0),COALESCE(MAX(accepted_through),0) FROM federation_relay_cursors WHERE binding_uid=$1 AND stream=$2`, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent).Scan(&emittedCursor, &acceptedCursor))
	require.Zero(t, emittedCursor)
	require.Zero(t, acceptedCursor)
	var emitted int
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_relay_outbox WHERE binding_uid=$1 AND emitted=1`, grant.Enrollment.RelayBindingUID).Scan(&emitted))
	require.Zero(t, emitted)
	removeFeature()
	first, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, creation.UID, first[0].SourceUID)
	require.Equal(t, creation.ContentHash, first[0].SourceHash)
	source, err := db.DecodeRelaySourceEvent(first[0].Body)
	require.NoError(t, err)
	require.Equal(t, db.RemoteEventFromStored(creation), source)
	retry, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.NoError(t, err)
	require.Equal(t, first, retry)
	require.Error(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence+100, "arbitrary"), "cannot acknowledge an unoffered maximum")
	require.Error(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 2, db.RelayStreamEvent, first[0].Sequence, first[0].Digest), "stale/wrong epoch cannot advance")
	insertFeature()
	require.ErrorIs(t, store.AckRelayDeliveries(peerCtx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence, first[0].Digest), db.ErrUnsupportedEventFeatures)
	var acknowledged int
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_relay_outbox WHERE binding_uid=$1 AND acknowledged=1`, grant.Enrollment.RelayBindingUID).Scan(&acknowledged))
	require.Zero(t, acknowledged)
	var acknowledgedCursor int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COALESCE(MAX(acknowledged_through),0) FROM federation_relay_cursors WHERE binding_uid=$1 AND stream=$2`, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent).Scan(&acknowledgedCursor))
	require.Zero(t, acknowledgedCursor)
	removeFeature()
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence, first[0].Digest))
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence, first[0].Digest), "lost ack reply is idempotent")
	second, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, commentEvent.UID, second[0].SourceUID)
	receipts, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
	require.NoError(t, err)
	require.Len(t, receipts, 2, "receipt stream remains independent of event acknowledgements")
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, second[0].Sequence, second[0].Digest))
	empty, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Empty(t, empty)
	// A legacy direct-spoke baseline still carries portable snapshots, while its
	// project.federation_enabled control record stays local to that transport.
	_, changed, err := store.RefreshProjectFederationBaseline(ctx, project.ID, "federation")
	require.NoError(t, err)
	require.True(t, changed)
	baseline, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, 1, len(baseline), "legacy control event must not enter relay delivery")
	baselineSource, err := db.DecodeRelaySourceEvent(baseline[0].Body)
	require.NoError(t, err)
	require.Equal(t, "issue.snapshot", baselineSource.Type)
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, baseline[0].Sequence, baseline[0].Digest))
	// A source/proof failure must not leave either a projection or queued intent.
	_, wrongPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, _, err = store.CreateIssue(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: wrongPrivate}, "member"), db.CreateIssueParams{ProjectID: project.ID, Title: "Must roll back", Author: "assistant"})
	require.Error(t, err)
	empty, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Empty(t, empty)
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
	require.Error(t, err, "revoke suppresses future queued proof disclosure")
}
