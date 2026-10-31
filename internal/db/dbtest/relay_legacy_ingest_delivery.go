package dbtest

import (
	"encoding/json/jsontext"
	"strings"
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

// RunRelayDefersUnresolvedLegacyIntroductions models linked adoption snapshots
// arriving in separate chunks. Retain the first introduction without offering
// its unresolved references or letting a root update overtake it.
func RunRelayDefersUnresolvedLegacyIntroductions(t *testing.T, store db.Storage) {
	runRelayDeferredLegacyIntroduction(t, store, false)
}

// RunRelayWithholdsResolvedPrivateIntroduction checks that an unresolved peer
// later found in another project never releases the retained source bytes.
func RunRelayWithholdsResolvedPrivateIntroduction(t *testing.T, store db.Storage) {
	runRelayDeferredLegacyIntroduction(t, store, true)
}

func runRelayDeferredLegacyIntroduction(t *testing.T, store db.Storage, private bool) {
	t.Helper()
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "deferred-legacy-ingest")
	peer, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion,
	})
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	firstUID, err := uid.New()
	require.NoError(t, err)
	secondUID, err := uid.New()
	require.NoError(t, err)
	first := newRemoteEvent(t, fixture.project, &firstUID, "issue.snapshot", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+firstUID+`","title":"First adopted issue","author":"spoke","status":"open","metadata":{},"comments":[],"labels":[],"links":[{"type":"blocks","to_issue_uid":"`+secondUID+`","author":"spoke"}]}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: fixture.project.ID, SpokeInstanceUID: spoke,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: first}},
	})
	require.NoError(t, err)
	issue, err := store.IssueByUID(ctx, firstUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	metadata, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "system", Patch: map[string]jsontext.Value{"notify.member": jsontext.Value(`"Scheduled date reached"`)},
	})
	require.NoError(t, err)
	blocked, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Empty(t, blocked, "neither unresolved introduction nor dependent updates may be offered yet")
	if private {
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, UID: secondUID, Title: "Private resolved endpoint", Author: "member"})
		require.NoError(t, err)
		blocked, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
		require.NoError(t, err)
		require.Empty(t, blocked, "a newly resolved private peer cannot release the source or its dependent update")
		return
	}
	second := newRemoteEvent(t, fixture.project, &secondUID, "issue.snapshot", "spoke", spoke, 701, jsontext.Value(`{"uid":"`+secondUID+`","title":"Later adopted peer","author":"spoke","status":"open","metadata":{},"comments":[],"labels":[],"links":[{"type":"blocks","to_issue_uid":"`+firstUID+`","incoming":true,"author":"spoke"}]}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: fixture.project.ID, SpokeInstanceUID: spoke,
		Events: []db.FederationIngestEvent{{SourceEventID: 2, Event: second}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{secondUID}, linkTargetUIDs(t, store, issue.ID))
	offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	var sources []string
	for _, envelope := range offered {
		sources = append(sources, envelope.SourceUID)
		if envelope.SourceUID == first.EventUID {
			require.Equal(t, first.ContentHash, envelope.SourceHash, "release the original retained introduction")
		}
	}
	require.Equal(t, []string{first.EventUID, metadata.Event.UID, second.EventUID}, sources)
	replayed, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, replayed, "released offers keep immutable replay identity")
}

// RunRelayWithholdsUpdateBehindPrivateIntroduction checks that an introduction
// referencing an already-known private endpoint stays queued as a withheld
// barrier. A root metadata update for the introduced issue must not pass the
// withheld introduction and reach the relay without it.
func RunRelayWithholdsUpdateBehindPrivateIntroduction(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "private-introduction-barrier")
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
	metadata, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "system", Patch: map[string]jsontext.Value{"notify.member": jsontext.Value(`"Scheduled date reached"`)},
	})
	require.NoError(t, err)
	require.NotEmpty(t, metadata.Event.UID)
	offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	for _, envelope := range offered {
		require.NotEqual(t, metadata.Event.UID, envelope.SourceUID,
			"an update cannot overtake its withheld introduction")
	}
	require.Empty(t, offered, "the withheld introduction holds its dependent update back")
}

// RunRelayForwardsCloseWithDivergentParentShortID checks that a legacy close
// forwards on the close-time parent UID alone. Short-ID collision resolution
// can renumber a parent differently on each replica, so the close-time parent
// short ID is a display hint, never same-project membership.
func RunRelayForwardsCloseWithDivergentParentShortID(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "close-divergent-parent-short-id")
	parent, parentCreated, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Local parent", Author: "member"})
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
	created := newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+issueUID+`","short_id":"`+strings.ToLower(issueUID[len(issueUID)-4:])+`","title":"Legacy child","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	closed := newRemoteEvent(t, fixture.project, &issueUID, "issue.closed", "spoke", spoke, 701, jsontext.Value(`{"issue_uid":"`+issueUID+`","reason":"done","closed_at":"2026-05-23T12:01:00.000Z","parent_uid":"`+parent.UID+`","parent_short_id":"zz99"}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: fixture.project.ID, SpokeInstanceUID: spoke,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: created}, {SourceEventID: 2, Event: closed}},
	})
	require.NoError(t, err)
	offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	var sources []string
	for _, envelope := range offered {
		sources = append(sources, envelope.SourceUID)
	}
	require.Equal(t, []string{parentCreated.UID, created.EventUID, closed.EventUID}, sources,
		"forward the same-project close on the parent UID alone")
}
