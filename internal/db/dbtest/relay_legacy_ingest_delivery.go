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
	t.Run("creation before update", func(t *testing.T) {
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
	})
	t.Run("snapshot peer arrives in later batch", func(t *testing.T) {
		ctx := t.Context()
		fixture := newRelayLinkBoundaryFixture(t, store, "chunked-legacy-ingest")
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
		peerUID, err := uid.New()
		require.NoError(t, err)
		first := newRemoteEvent(t, fixture.project, &issueUID, "issue.snapshot", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+issueUID+`","title":"Snapshot with deferred peer","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"`+peerUID+`","author":"spoke"}]}`))
		second := newRemoteEvent(t, fixture.project, &peerUID, "issue.snapshot", "spoke", spoke, 701, jsontext.Value(`{"uid":"`+peerUID+`","title":"Later snapshot peer","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
		for i, event := range []db.RemoteEvent{first, second} {
			result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{
				ProjectID: fixture.project.ID, SpokeInstanceUID: spoke,
				Events: []db.FederationIngestEvent{{SourceEventID: int64(i + 1), Event: event}},
			})
			require.NoError(t, err)
			require.Equal(t, 1, result.Accepted)
			offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			if i == 0 {
				require.Len(t, offered, 1, "an unknown peer must not withhold the snapshot")
				require.Equal(t, first.EventUID, offered[0].SourceUID)
			} else {
				var sources []string
				for _, envelope := range offered {
					sources = append(sources, envelope.SourceUID)
				}
				require.Contains(t, sources, second.EventUID)
			}
		}
	})
	runRelayRejectsLegacyCrossProjectEvents(t, store)
}

func runRelayRejectsLegacyCrossProjectEvents(t *testing.T, store db.Storage) {
	for _, kind := range []string{"creation with sibling link", "close parent", "unresolved parent UID", "unresolved parent short ID", "sibling parent short ID"} {
		t.Run("legacy boundary "+kind, func(t *testing.T) {
			ctx := t.Context()
			fixture := newRelayLinkBoundaryFixture(t, store, "legacy-boundary-"+kind)
			private, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, Title: "Sibling endpoint", Author: "member"})
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
			created := newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+issueUID+`","title":"Legacy task","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
			push := db.FederationIngestParams{ProjectID: fixture.project.ID, SpokeInstanceUID: spoke}
			var events []db.FederationIngestEvent
			if kind == "creation with sibling link" {
				created = newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+issueUID+`","title":"Legacy task","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"`+private.UID+`","author":"spoke"}]}`))
				updated := newRemoteEvent(t, fixture.project, &issueUID, "issue.updated", "spoke", spoke, 701, jsontext.Value(`{"title":"Later update"}`))
				events = []db.FederationIngestEvent{{SourceEventID: 1, Event: created}, {SourceEventID: 2, Event: updated}}
			} else {
				push.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: created}}
				_, err = store.IngestFederationEvents(ctx, push)
				require.NoError(t, err)
				offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Len(t, offered, 1)
				require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, offered[0].Sequence, offered[0].Digest))
				payload := jsontext.Value(`{"reason":"done","parent_uid":"` + private.UID + `","parent_short_id":"` + private.ShortID + `"}`)
				switch kind {
				case "unresolved parent UID":
					payload = jsontext.Value(`{"reason":"done","parent_uid":"` + peer + `"}`)
				case "unresolved parent short ID":
					payload = jsontext.Value(`{"reason":"done","parent_short_id":"unknown"}`)
				case "sibling parent short ID":
					payload = jsontext.Value(`{"reason":"done","parent_short_id":"` + private.ShortID + `"}`)
				}
				closed := newRemoteEvent(t, fixture.project, &issueUID, "issue.closed", "spoke", spoke, 701, payload)
				events = []db.FederationIngestEvent{{SourceEventID: 2, Event: closed}}
			}
			push.Events = events
			result, err := store.IngestFederationEvents(ctx, push)
			if kind == "unresolved parent UID" || kind == "unresolved parent short ID" {
				require.NoError(t, err)
				require.Equal(t, 1, result.Accepted)
				retained, err := store.EventsByUIDs(ctx, fixture.project.ID, []string{events[0].Event.EventUID})
				require.NoError(t, err)
				require.Len(t, retained, 1)
				offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Len(t, offered, 1)
				require.Equal(t, events[0].Event.EventUID, offered[0].SourceUID)
				issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "closed", issue.Status)
				return
			}
			require.ErrorIs(t, err, db.ErrFederationIngestValidation)
			require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
			offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Empty(t, offered, "refused events must never reach a relay")
			_, err = store.EventsByUIDs(ctx, fixture.project.ID, []string{events[0].Event.EventUID})
			require.ErrorIs(t, err, db.ErrNotFound)
			issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
			if kind == "creation with sibling link" {
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, "open", issue.Status)
				if kind != "close parent" {
					return
				}
				_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: fixture.project.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "Short ID collision", Author: "member",
				})
				require.NoError(t, err)
				parentUID := "01HZNQ7VFPK1XGD8R5MABXD4EX"
				parent := newRemoteEvent(t, fixture.project, &parentUID, "issue.created", "spoke", spoke, 702, jsontext.Value(`{"uid":"`+parentUID+`","short_id":"d4ex","title":"Parent","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
				closed := newRemoteEvent(t, fixture.project, &issueUID, "issue.closed", "spoke", spoke, 703, jsontext.Value(`{"reason":"done","parent_uid":"`+parentUID+`","parent_short_id":"d4ex"}`))
				push.Events = []db.FederationIngestEvent{{SourceEventID: 2, Event: parent}, {SourceEventID: 3, Event: closed}}
				result, err := store.IngestFederationEvents(ctx, push)
				require.NoError(t, err, "a same-project parent UID must resolve despite its historical short ID")
				require.Equal(t, 2, result.Accepted)
				resolvedParent, err := store.IssueByUID(ctx, parentUID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "xd4ex", resolvedParent.ShortID)
				issue, err = store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "closed", issue.Status)
				offered, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				var sources []string
				for _, envelope := range offered {
					sources = append(sources, envelope.SourceUID)
				}
				require.Contains(t, sources, closed.EventUID, "the accepted close must reach the relay")
			}
		})
	}
}

// RunRelayAcceptsLegacyCrossProjectRemovals covers cleanup a legacy spoke
// pushes after relay enrollment. Removing a sibling edge cannot add a boundary
// crossing, so the owner accepts it while keeping the historical endpoint local.
func RunRelayAcceptsLegacyCrossProjectRemovals(t *testing.T, store db.Storage) {
	for _, kind := range []string{"unlink", "removal-only links_changed"} {
		t.Run("legacy "+kind+" of sibling", func(t *testing.T) {
			ctx := t.Context()
			fixture := newRelayLinkBoundaryFixture(t, store, "legacy-removal-"+kind)
			private, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, Title: "Sibling endpoint", Author: "member"})
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
			created := newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "spoke", spoke, 700, jsontext.Value(`{"uid":"`+issueUID+`","title":"Legacy task","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
			push := db.FederationIngestParams{ProjectID: fixture.project.ID, SpokeInstanceUID: spoke, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: created}}}
			_, err = store.IngestFederationEvents(ctx, push)
			require.NoError(t, err)
			offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Len(t, offered, 1)
			require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, offered[0].Sequence, offered[0].Digest))
			removal := newRemoteEvent(t, fixture.project, &issueUID, "issue.unlinked", "spoke", spoke, 701, jsontext.Value(`{"type":"related","from_uid":"`+issueUID+`","to_uid":"`+private.UID+`"}`))
			if kind == "removal-only links_changed" {
				removal = newRemoteEvent(t, fixture.project, &issueUID, "issue.links_changed", "spoke", spoke, 701, jsontext.Value(`{"related_removed_uids":["`+private.UID+`"]}`))
			}
			push.Events = []db.FederationIngestEvent{{SourceEventID: 2, Event: removal}}
			result, err := store.IngestFederationEvents(ctx, push)
			require.NoError(t, err, "removing a sibling edge must stay admissible after relay enrollment")
			require.Equal(t, 1, result.Accepted)
			retained, err := store.EventsByUIDs(ctx, fixture.project.ID, []string{removal.EventUID})
			require.NoError(t, err)
			require.Len(t, retained, 1)
			offered, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Empty(t, offered, "a removal naming a sibling endpoint must stay owner-local")
		})
	}
}
