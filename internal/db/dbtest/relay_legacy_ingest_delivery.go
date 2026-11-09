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
}

// RunLegacyRelayAdmission separates fresh additions from retained delivery and cleanup.
func RunLegacyRelayAdmission(t *testing.T, store db.Storage) {
	for kind, modes := range map[string][]string{
		"creation":           {"sibling", "same project", "relayed peer", "revoked", "missing", "retained"},
		"snapshot":           {"sibling", "missing"},
		"single link":        {"sibling"},
		"composite":          {"sibling"},
		"parent":             {"sibling"},
		"parent short ID":    {"sibling", "same project", "relayed peer", "missing"},
		"parent empty UID":   {"sibling"},
		"duplicate-of":       {"sibling", "same project", "missing"},
		"superseded-by":      {"sibling", "same project", "missing"},
		"malformed evidence": {"sibling"},
		"unlink":             {"sibling"},
		"composite cleanup":  {"sibling"},
		"parent replacement": {"sibling"},
	} {
		for _, mode := range modes {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				fixture := newRelayLinkBoundaryFixture(t, store, "legacy-"+kind+"-"+mode)
				if mode == "relayed peer" {
					fixture.project, fixture.sibling = fixture.sibling, fixture.project
				}
				peerProject := fixture.sibling
				if mode == "same project" {
					peerProject = fixture.project
				}
				peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peerProject.ID, Title: "Peer", Author: "member"})
				require.NoError(t, err)
				if mode == "missing" {
					peer.UID, err = uid.New()
					require.NoError(t, err)
					peer.ShortID = strings.ToLower(peer.UID)
				}
				primary, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Primary", Author: "member"})
				require.NoError(t, err)
				relayProject := fixture.project
				if mode == "relayed peer" {
					relayProject = fixture.sibling
				}
				instance, err := uid.New()
				require.NoError(t, err)
				grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: relayProject.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: instance, ProtocolVersion: db.RelayProtocolVersion})
				require.NoError(t, err)
				if mode == "revoked" || mode == "retained" {
					require.NoError(t, store.RevokeFederationEnrollment(ctx, grant.Enrollment.ID))
				}
				eventType := "issue.linked"
				payload := `{"type":"related","from_uid":"` + primary.UID + `","to_uid":"` + peer.UID + `"}`
				switch kind {
				case "creation", "snapshot":
					primary.UID, err = uid.New()
					require.NoError(t, err)
					eventType = "issue.created"
					if kind == "snapshot" {
						eventType = "issue.snapshot"
					}
					payload = `{"uid":"` + primary.UID + `","title":"Legacy creation","body":"","author":"member","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"` + peer.UID + `","author":"member"}]}`
				case "composite", "composite cleanup", "parent replacement":
					eventType = "issue.links_changed"
					removal, err := uid.New()
					require.NoError(t, err)
					payload = `{"related_added_uids":["` + peer.UID + `"],"related_removed_uids":["` + removal + `"]}`
					if kind == "composite cleanup" {
						payload = `{"related_removed_uids":["` + peer.UID + `"]}`
					}
					if kind == "parent replacement" {
						replacement, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Replacement parent", Author: "member"})
						require.NoError(t, err)
						payload = `{"parent_removed_uid":"` + peer.UID + `","parent_set_uid":"` + replacement.UID + `"}`
					}
				case "parent":
					eventType = "issue.closed"
					payload = `{"reason":"done","parent_uid":"` + peer.UID + `","parent_short_id":"` + peer.ShortID + `"}`
				case "parent short ID", "parent empty UID":
					eventType = "issue.closed"
					payload = `{"reason":"done","parent_short_id":"` + peer.ShortID + `"}`
					if kind == "parent empty UID" {
						payload = `{"reason":"done","parent_uid":"","parent_short_id":"` + peer.ShortID + `"}`
					}
				case "duplicate-of", "superseded-by":
					eventType = "issue.closed"
					ref := peer.UID
					if kind == "superseded-by" {
						ref = peerProject.Name + "#" + peer.ShortID
					}
					payload = `{"reason":"done","evidence":[{"type":"` + kind + `","issue_ref":"` + ref + `"}]}`
				case "malformed evidence":
					eventType = "issue.closed"
					payload = `{"reason":"done","evidence":{}}`
				case "unlink":
					eventType = "issue.unlinked"
				}
				spoke, err := uid.New()
				require.NoError(t, err)
				event := newRemoteEvent(t, fixture.project, &primary.UID, eventType, "member", spoke, 900, jsontext.Value(payload))
				push := db.FederationIngestParams{ProjectID: fixture.project.ID, SpokeInstanceUID: spoke, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
				var precedingUID string
				if kind == "creation" && mode == "sibling" {
					precedingUID, err = uid.New()
					require.NoError(t, err)
					valid := newRemoteEvent(t, fixture.project, &precedingUID, "issue.created", "member", spoke, 899, jsontext.Value(`{"uid":"`+precedingUID+`","title":"Earlier valid creation","author":"member","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
					push.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: valid}, {SourceEventID: 2, Event: event}}
				}
				authorizedProjects := []string{fixture.project.UID}
				if mode == "revoked" || mode == "retained" || kind == "unlink" || kind == "composite cleanup" || kind == "parent replacement" {
					// Accepted cross-project writes still need authority over both endpoints.
					authorizedProjects = append(authorizedProjects, peerProject.UID)
				}
				result, err := store.IngestFederationEvents(db.WithAuthorizedProjects(ctx, authorizedProjects), push)
				if kind == "malformed evidence" {
					require.ErrorIs(t, err, db.ErrFederationIngestValidation)
					return
				}
				refuse := (mode == "sibling" || mode == "relayed peer") && kind != "unlink" && kind != "composite cleanup" && kind != "parent replacement"
				if refuse {
					require.ErrorIs(t, err, db.ErrFederationIngestValidation)
					require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
					require.ErrorContains(t, err, event.EventUID)
					require.NotContains(t, err.Error(), peer.UID)
					_, err = store.EventsByUIDs(ctx, fixture.project.ID, []string{event.EventUID})
					require.ErrorIs(t, err, db.ErrNotFound)
					if precedingUID != "" {
						_, err = store.EventsByUIDs(ctx, fixture.project.ID, []string{push.Events[0].Event.EventUID})
						require.ErrorIs(t, err, db.ErrNotFound)
						_, err = store.IssueByUID(ctx, precedingUID, db.IncludeDeletedYes)
						require.ErrorIs(t, err, db.ErrNotFound, "the earlier valid event rolls back with the refused batch")
					}
					return
				}
				require.NoError(t, err)
				require.Equal(t, 1, result.Accepted)
				stored, err := store.EventsByUIDs(ctx, fixture.project.ID, []string{event.EventUID})
				require.NoError(t, err)
				require.Len(t, stored, 1)
				require.Equal(t, event.ContentHash, stored[0].ContentHash)
				if mode == "retained" {
					issue, err := store.IssueByUID(ctx, primary.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					links, err := store.LinksByIssue(ctx, issue.ID)
					require.NoError(t, err)
					for _, link := range links {
						_, err := store.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: issue.ID, Actor: "member", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
						require.NoError(t, err)
					}
					_, err = store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: relayProject.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: instance, ProtocolVersion: db.RelayProtocolVersion})
					require.NoError(t, err)
				}
				retry, err := store.IngestFederationEvents(ctx, push)
				require.NoError(t, err)
				require.Equal(t, 1, retry.Duplicates)
			})
		}
	}
}

// RunLegacyRelayParentUIDCollision resolves the parent UID despite a colliding source-project short ID.
func RunLegacyRelayParentUIDCollision(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "legacy-parent-collision")
	_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "Short ID collision", Author: "member"})
	require.NoError(t, err)
	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "Sibling parent", Author: "member"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Child", Author: "member"})
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	_, err = store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: spoke, ProtocolVersion: db.RelayProtocolVersion})
	require.NoError(t, err)
	closed := newRemoteEvent(t, fixture.project, &child.UID, "issue.closed", "member", spoke, 702, jsontext.Value(`{"reason":"done","parent_uid":"`+parent.UID+`","parent_short_id":"d4ex"}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: fixture.project.ID, SpokeInstanceUID: spoke, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: closed}}})
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)

	closed = newRemoteEvent(t, fixture.project, &child.UID, "issue.closed", "member", spoke, 703, jsontext.Value(`{"reason":"done","parent_short_id":"d4ex"}`))
	result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: fixture.project.ID, SpokeInstanceUID: spoke, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: closed}}})
	require.NoError(t, err, "an ambiguous short ID does not establish a crossing")
	require.Equal(t, 1, result.Accepted)
}
