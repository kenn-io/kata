package dbtest

import (
	"crypto/ed25519"
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
