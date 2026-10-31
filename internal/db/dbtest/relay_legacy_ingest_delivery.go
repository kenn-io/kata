package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayOffersLegacyIngestedEvents covers a direct spoke's creation followed
// by a root-local notification. The relay must receive the introduction first,
// retaining original source identity without manufacturing root attribution.
func RunRelayOffersLegacyIngestedEvents(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "legacy-ingest-relay")
	peer, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion,
	})
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	created := newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+issueUID+`","title":"Pushed by a direct spoke","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	push := db.FederationIngestParams{
		ProjectID: fixture.project.ID, SpokeInstanceUID: spoke, BoundActor: "spoke",
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: created}},
	}
	result, err := store.IngestFederationEvents(db.WithRootAttribution(ctx, fixture.signer, "relay-member"), push)
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, err = store.EntityAttribution(ctx, fixture.project.UID, "issue", issueUID)
	require.ErrorIs(t, err, db.ErrNotFound, "legacy source labels are not root-certified creation proof")
	metadata, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "system", Patch: map[string]jsontext.Value{"notify.member": jsontext.Value(`"Scheduled date reached"`)},
	})
	require.NoError(t, err)
	offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	var sources []string
	for _, envelope := range offered {
		sources = append(sources, envelope.SourceUID)
		if envelope.SourceUID == created.EventUID {
			source, err := db.DecodeRelaySourceEvent(envelope.Body)
			require.NoError(t, err)
			require.Equal(t, created.ContentHash, source.ContentHash)
			require.Equal(t, spoke, source.OriginInstanceUID)
			require.Equal(t, []string{spoke, store.InstanceUID()}, envelope.Path)
		}
	}
	require.Equal(t, []string{created.EventUID, metadata.Event.UID}, sources, "the root must offer the spoke introduction before its own later update")
	receipts, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 100)
	require.NoError(t, err)
	require.Empty(t, receipts, "legacy acceptance must not invent root-certified attribution")
	retry, err := store.IngestFederationEvents(ctx, push)
	require.NoError(t, err)
	require.Zero(t, retry.Accepted)
	require.Equal(t, 1, retry.Duplicates)
	replayed, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, replayed, "a duplicate push must preserve the original delivery identities")
}

// RunRelayWithholdsPrivateLegacyIngestedEvents checks that accepting a legacy
// composite event does not disclose its original private endpoint to relays.
// The owner keeps the complete event and current link; neither is rewritten.
func RunRelayWithholdsPrivateLegacyIngestedEvents(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "private-legacy-ingest")
	private, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, Title: "Private endpoint", Author: "member"})
	require.NoError(t, err)
	peer, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion,
	})
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	created := newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+issueUID+`","title":"Private legacy boundary","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"`+private.UID+`","author":"spoke"}]}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: fixture.project.ID, SpokeInstanceUID: spoke,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: created}},
	})
	require.NoError(t, err)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, []string{private.UID}, linkTargetUIDs(t, store, issue.ID))
	events, err := store.EventsByUIDs(ctx, fixture.project.ID, []string{created.EventUID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	for _, event := range events {
		require.Equal(t, created.ContentHash, event.ContentHash)
		require.Contains(t, event.Payload, private.UID)
	}
	offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Empty(t, offered, "the original cross-project event stays owner-local")
}
