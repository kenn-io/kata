package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedParentWithoutReferenceController(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	route, err := store.CreateProject(ctx, "ordinary-route")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "ordinary-other")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, route.ID, "worker")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Child", Author: "worker"})
	require.NoError(t, err)
	current, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Current parent", Author: "worker"})
	require.NoError(t, err)
	queued, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Queued parent", Author: "worker"})
	require.NoError(t, err)
	_, linkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: current.ID, Type: "parent", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: child.ID, FromUID: child.UID, FromShortID: child.ShortID, ToUID: current.UID, ToShortID: current.ShortID, Actor: "worker"})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &route.ID, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]string{"type": "parent", "from_uid": child.UID, "to_uid": queued.UID})
	require.NoError(t, err)
	stale := newRemoteEvent(t, route, &child.UID, "issue.unlinked", "worker", origin, linkEvent.HLCPhysicalMS-1, payload)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: route.ID, SpokeInstanceUID: origin, BoundActor: "worker", FederationEnrollmentID: grant.Enrollment.ID, EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 2, Event: stale}}})
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	require.ErrorContains(t, err, "no longer has")
	parent, err := store.ParentOf(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, current.ID, parent.ToIssueID)
	return nil
}

// Fresh source-stream work can alter an issue which now belongs elsewhere;
// credential, actor, and host authority follow that current project.
func checkFederatedFreshCurrentProjectAuthority(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"missing-proof", "link-peer", "pull-only", "revoked", "wrong-actor", "wrong-spoke", "actor-denied", "host-denied", "authorized", "global-grant"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, _ := federatedRehomeFixture(t, store, "current-auth-"+kind)
			_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			origin, err := uid.New()
			require.NoError(t, err)
			sourceProjectID := &source.ID
			if kind == "global-grant" {
				sourceProjectID = nil
			}
			routeGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: sourceProjectID, Capabilities: "push,pull", Actor: "worker"})
			require.NoError(t, err)
			targetActor, targetOrigin, capabilities := "worker", origin, "push,pull"
			if kind == "wrong-actor" {
				targetActor = "other-worker"
			}
			if kind == "wrong-spoke" {
				targetOrigin, err = uid.New()
				require.NoError(t, err)
			}
			if kind == "pull-only" {
				capabilities = "pull"
			}
			targetGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: targetOrigin, ProjectID: &target.ID, Capabilities: capabilities, Actor: targetActor})
			require.NoError(t, err)
			if kind == "revoked" {
				require.NoError(t, store.RevokeFederationEnrollment(ctx, targetGrant.Enrollment.ID))
			}
			if kind == "actor-denied" {
				team, _, err := store.CreateTeam(ctx, "current-auth-team", "admin")
				require.NoError(t, err)
				_, err = store.SetTeamMembership(ctx, team.UID, "other-worker", true, "admin")
				require.NoError(t, err)
				_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: target.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
				require.NoError(t, err)
			}
			payload, err := json.Marshal(map[string]any{"uid": issue.UID, "title": "Fresh former-project update"})
			require.NoError(t, err)
			event := newRemoteEvent(t, source, &issue.UID, "issue.updated", "worker", origin, time.Now().Add(time.Hour).UnixMilli(), jsontext.Value(payload))
			if kind == "link-peer" {
				primary, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Source primary", Author: "worker"})
				require.NoError(t, err)
				payload, err = json.Marshal(map[string]any{"from_uid": primary.UID, "to_uid": issue.UID, "type": "related"})
				require.NoError(t, err)
				event = newRemoteEvent(t, source, &primary.UID, "issue.linked", "worker", origin, time.Now().Add(time.Hour).UnixMilli(), jsontext.Value(payload))
				event.RelatedIssueUID = &issue.UID
				event.ContentHash, err = db.RemoteEventContentHash(event)
				require.NoError(t, err)
			}
			denied := errors.New("current host project denied")
			currentHostChecked := false
			params := db.FederationIngestParams{ProjectID: source.ID, FederationEnrollmentID: routeGrant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
			if kind != "missing-proof" && kind != "link-peer" {
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: targetGrant.Enrollment.ID}
			}
			params.AuthorizeMove = func(_ context.Context, _ db.Transaction, grant db.FederationEnrollment, project db.Project) error {
				if project.UID == target.UID {
					currentHostChecked = true
					require.Equal(t, "worker", grant.Actor)
					if kind == "host-denied" {
						return denied
					}
				}
				return nil
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			result, err := store.IngestFederationEvents(ctx, params)
			allowed := kind == "authorized" || kind == "global-grant"
			if !allowed {
				require.Error(t, err, "fresh work requires authority for every affected issue's current project")
				if kind == "host-denied" {
					require.ErrorIs(t, err, denied)
					require.True(t, currentHostChecked)
				} else if kind != "actor-denied" {
					// The route credential is valid, so a missing or unusable
					// enrollment for the issue's current project gets its own
					// error instead of blaming the credential. Access policy
					// denial is separate.
					var notEnrolled *db.FederationProjectNotEnrolledError
					require.ErrorAs(t, err, &notEnrolled)
					require.Equal(t, issue.UID, notEnrolled.IssueUID)
				}
				after, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, before, after)
				retained, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, issue.Title, retained.Title)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, result.Accepted)
			require.True(t, currentHostChecked)
			changed, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, target.ID, changed.ProjectID)
			require.Equal(t, "Fresh former-project update", changed.Title)
			stored, err := store.EventsByUIDs(ctx, source.ID, []string{event.EventUID})
			require.NoError(t, err)
			require.Equal(t, event.ContentHash, stored[0].ContentHash, "authority checks preserve the original event envelope/hash")
			require.NoError(t, store.RevokeFederationEnrollment(ctx, targetGrant.Enrollment.ID))
			params.MoveSourceEnrollmentIDs = nil
			currentHostChecked = false
			duplicate, err := store.IngestFederationEvents(ctx, params)
			require.NoError(t, err, "persisted duplicates do not need new current-project authority")
			require.Equal(t, 1, duplicate.Duplicates)
			require.Zero(t, duplicate.Accepted)
			require.False(t, currentHostChecked)
		})
	}
	return nil
}

// A host-approved link only proves permission to reference the peer. A later
// edit of that peer or a move from its project needs ordinary source authority.
func checkFederatedReferenceBatchAuthority(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, route, peer, move := federatedRehomeFixture(t, store, "peer-batch")
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Child", Author: "worker"})
	require.NoError(t, err)
	origin := move.OriginInstanceUID
	routeGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &route.ID, SpokeInstanceUID: origin, Capabilities: "push", Actor: "worker"})
	require.NoError(t, err)
	peerGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &source.ID, SpokeInstanceUID: origin, Capabilities: "push", Actor: "peer-member"})
	require.NoError(t, err)
	at := move.HLCPhysicalMS
	payload, err := json.Marshal(map[string]string{"parent_set_uid": peer.UID, "updated_at": time.UnixMilli(at).UTC().Format(time.RFC3339Nano)})
	require.NoError(t, err)
	link := newRemoteEvent(t, route, &child.UID, "issue.links_changed", "worker", origin, at, payload)
	// The route retains history for the delayed edit after the issue moves away.
	delayed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Former route issue", Author: "worker"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: delayed.ID, FromProjectID: route.ID, ToProjectID: source.ID, IfMatchRev: delayed.Revision, Actor: "worker"})
	require.NoError(t, err)
	edit := newRemoteEvent(t, route, &delayed.UID, "issue.updated", "worker", origin, at+1, jsontext.Value(`{"title":"Delayed edit"}`))
	params := db.FederationIngestParams{ProjectID: route.ID, FederationEnrollmentID: routeGrant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, MoveSourceEnrollmentIDs: map[string]int64{source.UID: peerGrant.Enrollment.ID}, AuthorizeReference: func(_ context.Context, _ db.Transaction, grant db.FederationEnrollment, project db.Project) error {
		require.Equal(t, peerGrant.Enrollment.ID, grant.ID)
		require.Equal(t, source.UID, project.UID)
		return nil
	}}
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for _, sibling := range []db.RemoteEvent{edit, move} {
		params.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: link}, {SourceEventID: 2, Event: sibling}}
		_, err := store.IngestFederationEvents(ctx, params)
		require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	params.Events = params.Events[:1]
	result, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	parent, err := store.ParentOf(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, peer.ID, parent.ToIssueID)

	directChild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Direct link", Author: "worker"})
	require.NoError(t, err)
	directPayload, err := json.Marshal(map[string]string{"from_uid": directChild.UID, "to_uid": peer.UID, "type": "parent"})
	require.NoError(t, err)
	direct := newRemoteEvent(t, route, &directChild.UID, "issue.linked", "worker", origin, at+1, directPayload)
	direct.RelatedIssueUID = &peer.UID
	direct.ContentHash, err = db.RemoteEventContentHash(direct)
	require.NoError(t, err)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 2, Event: direct}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	parent, err = store.ParentOf(ctx, directChild.ID)
	require.NoError(t, err)
	require.Equal(t, peer.ID, parent.ToIssueID)

	peerChild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Peer child", Author: "peer-member"})
	require.NoError(t, err)
	forgedPayload, err := json.Marshal(map[string]string{"from_uid": peerChild.UID, "to_uid": directChild.UID, "type": "parent"})
	require.NoError(t, err)
	forged := newRemoteEvent(t, route, &directChild.UID, "issue.linked", "worker", origin, at+2, forgedPayload)
	forged.RelatedIssueUID = &peerChild.UID
	forged.ContentHash, err = db.RemoteEventContentHash(forged)
	require.NoError(t, err)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: forged}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
	_, err = store.ParentOf(ctx, peerChild.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.EventsByUIDs(ctx, route.ID, []string{forged.EventUID})
	require.ErrorIs(t, err, db.ErrNotFound)

	linkedUID, err := uid.New()
	require.NoError(t, err)
	linkedDocument := issueDocumentFields(linkedUID, "worker")
	linkedDocument["links"] = []map[string]any{{"to_issue_uid": peerChild.UID, "type": "blocks"}}
	linkedSnapshot := newRemoteEvent(t, route, &linkedUID, "issue.snapshot", "worker", origin, at+2, marshalIssueDocument(t, linkedDocument))
	params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: linkedSnapshot}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	linkedIssue, err := store.IssueByUID(ctx, linkedUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := store.LinksByIssue(ctx, linkedIssue.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, peerChild.UID, links[0].ToIssueUID)

	for _, links := range [][]map[string]any{
		{{"to_issue_uid": peerChild.UID, "type": "parent", "incoming": true}},
		{{"to_issue_uid": peerChild.UID, "type": "parent", "incoming": true}, {"to_issue_uid": peerChild.UID, "type": "blocks"}},
	} {
		snapshotUID, err := uid.New()
		require.NoError(t, err)
		document := issueDocumentFields(snapshotUID, "worker")
		document["links"] = links
		snapshot := newRemoteEvent(t, route, &snapshotUID, "issue.snapshot", "worker", origin, at+2, marshalIssueDocument(t, document))
		params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: snapshot}}
		_, err = store.IngestFederationEvents(ctx, params)
		require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
		_, err = store.ParentOf(ctx, peerChild.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
	}

	// A parent URL can remove the same cross-project edge from its other end.
	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: peerChild.ID, ToIssueID: directChild.ID, Type: "parent", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: peerChild.ID, FromUID: peerChild.UID, FromShortID: peerChild.ShortID, ToUID: directChild.UID, ToShortID: directChild.ShortID, Actor: "worker"})
	require.NoError(t, err)
	parentUnlink, err := json.Marshal(map[string]string{"from_uid": directChild.UID, "to_uid": peerChild.UID, "link_from_uid": peerChild.UID, "link_to_uid": directChild.UID, "type": "parent"})
	require.NoError(t, err)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: newRemoteEvent(t, route, &directChild.UID, "issue.unlinked", "worker", origin, at+3, parentUnlink)}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	_, err = store.ParentOf(ctx, peerChild.ID)
	require.ErrorIs(t, err, db.ErrNotFound)

	// Both operations name the same peer and allow reference approval.
	mixedChild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Mixed child", Author: "worker"})
	require.NoError(t, err)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: newRemoteEvent(t, route, &mixedChild.UID, "issue.links_changed", "worker", origin, at+1, payload)}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	parent, err = store.ParentOf(ctx, mixedChild.ID)
	require.NoError(t, err)
	require.Equal(t, peer.ID, parent.ToIssueID)
	combinedPayload, err := json.Marshal(map[string]any{"parent_removed_uid": peer.UID, "related_added_uids": []string{peer.UID}})
	require.NoError(t, err)
	combined := newRemoteEvent(t, route, &mixedChild.UID, "issue.links_changed", "worker", origin, at+2, combinedPayload)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: combined}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	_, err = store.ParentOf(ctx, mixedChild.ID)
	require.ErrorIs(t, err, db.ErrNotFound)

	// A spoke that has not pulled this parent sees only its new parent.
	// Authorizing that addition cannot also authorize removing the hub's parent.
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "New parent", Author: "peer-member"})
	require.NoError(t, err)
	setSecond, err := json.Marshal(map[string]string{"parent_set_uid": second.UID})
	require.NoError(t, err)
	replacement := newRemoteEvent(t, route, &child.UID, "issue.links_changed", "worker", origin, at+3, setSecond)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 4, Event: replacement}}
	before, err = store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
	require.NotContains(t, err.Error(), peer.UID)
	require.ErrorContains(t, err, "This spoke needs push access to the affected issue's project with the same actor")
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	parent, err = store.ParentOf(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, peer.ID, parent.ToIssueID)

	// Two queued sets for a previously unparented child also displace a parent.
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "Other child", Author: "worker"})
	require.NoError(t, err)
	firstSet := newRemoteEvent(t, route, &other.UID, "issue.links_changed", "worker", origin, at+4, payload)
	lastSet := newRemoteEvent(t, route, &other.UID, "issue.links_changed", "worker", origin, at+5, setSecond)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 4, Event: firstSet}, {SourceEventID: 5, Event: lastSet}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
	_, err = store.ParentOf(ctx, other.ID)
	require.ErrorIs(t, err, db.ErrNotFound)

	// Setting and then clearing a route-local parent still displaces the
	// child's original hub parent, even if neither event names that parent.
	setLocal, err := json.Marshal(map[string]string{"parent_set_uid": other.UID})
	require.NoError(t, err)
	clearLocal, err := json.Marshal(map[string]any{"parent_removed_uid": other.UID, "related_added_uids": []string{peer.UID}})
	require.NoError(t, err)
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 4, Event: newRemoteEvent(t, route, &child.UID, "issue.links_changed", "worker", origin, at+6, setLocal)},
		{SourceEventID: 5, Event: newRemoteEvent(t, route, &child.UID, "issue.links_changed", "worker", origin, at+7, clearLocal)},
	}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
	parent, err = store.ParentOf(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, peer.ID, parent.ToIssueID)
	t.Run("combined-parent-and-blocks-removal", func(t *testing.T) {
		payload, err := json.Marshal(map[string]any{"parent_removed_uid": peer.UID, "blocks_removed_uids": []string{peer.UID}})
		require.NoError(t, err)
		params.Events = []db.FederationIngestEvent{{SourceEventID: 4, Event: newRemoteEvent(t, route, &child.UID, "issue.links_changed", "worker", origin, at+7, payload)}}
		_, err = store.IngestFederationEvents(ctx, params)
		require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
		parent, err := store.ParentOf(ctx, child.ID)
		require.NoError(t, err)
		require.Equal(t, peer.ID, parent.ToIssueID)
	})
	// A reference proof for a peer does not authorize removing a blocks link.
	blocksUnlink, err := json.Marshal(map[string]string{"from_uid": child.UID, "to_uid": peer.UID, "link_from_uid": child.UID, "link_to_uid": peer.UID, "type": "blocks"})
	require.NoError(t, err)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 4, Event: newRemoteEvent(t, route, &child.UID, "issue.unlinked", "worker", origin, at+7, blocksUnlink)}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
	unlinked, err := json.Marshal(map[string]string{"from_uid": child.UID, "to_uid": peer.UID, "link_from_uid": child.UID, "link_to_uid": peer.UID, "type": "parent"})
	require.NoError(t, err)
	linked, err := json.Marshal(map[string]string{"from_uid": child.UID, "to_uid": second.UID, "type": "parent"})
	require.NoError(t, err)
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 7, Event: newRemoteEvent(t, route, &child.UID, "issue.unlinked", "worker", origin, at+9, unlinked)},
		{SourceEventID: 8, Event: newRemoteEvent(t, route, &child.UID, "issue.linked", "worker", origin, at+10, linked)},
	}
	_, err = store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	parent, err = store.ParentOf(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, second.ID, parent.ToIssueID)

	return nil
}
