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

	// A parent URL can remove the same cross-project edge from its other end.
	_, localLinkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: peerChild.ID, ToIssueID: directChild.ID, Type: "parent", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: peerChild.ID, FromUID: peerChild.UID, FromShortID: peerChild.ShortID, ToUID: directChild.UID, ToShortID: directChild.ShortID, Actor: "worker"})
	require.NoError(t, err)
	parentUnlink, err := json.Marshal(map[string]string{"from_uid": directChild.UID, "to_uid": peerChild.UID, "link_from_uid": peerChild.UID, "link_to_uid": directChild.UID, "type": "parent"})
	require.NoError(t, err)
	// The unlink must follow the local link even when setup takes over a second.
	params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: newRemoteEvent(t, route, &directChild.UID, "issue.unlinked", "worker", origin, localLinkEvent.HLCPhysicalMS+1, parentUnlink)}}
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

// checkFederatedPartialNotEnrolledIngest verifies per-event refusal for
// unenrolled current projects, intra-batch causal dependency propagation,
// hard-error rollback, and spoke out-of-order acknowledgement folding.
func checkFederatedPartialNotEnrolledIngest(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, target, moved, _ := federatedRehomeFixture(t, store, "partial-not-enrolled")
	_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: moved.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: moved.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	stayingA, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Staying A", Author: "worker"})
	require.NoError(t, err)
	stayingB, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Staying B", Author: "worker"})
	require.NoError(t, err)
	movedComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: moved.ID, Author: "worker", Body: "comment on moved"})
	require.NoError(t, err)

	origin, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "worker",
	})
	require.NoError(t, err)
	at := time.Now().Add(time.Hour).UnixMilli()

	// Mixed batch: [before, stuck, after] accepts before and after while refusing stuck.
	beforeEv := newRemoteEvent(t, source, &stayingA.UID, "issue.updated", "worker", origin, at+1, jsontext.Value(`{"title":"Staying A updated"}`))
	stuckEv := newRemoteEvent(t, source, &moved.UID, "issue.updated", "worker", origin, at+2, jsontext.Value(`{"title":"Should stay refused"}`))
	afterEv := newRemoteEvent(t, source, &stayingB.UID, "issue.updated", "worker", origin, at+3, jsontext.Value(`{"title":"Staying B updated"}`))
	baseParams := db.FederationIngestParams{
		ProjectID:               source.ID,
		FederationEnrollmentID:  grant.Enrollment.ID,
		SpokeInstanceUID:        origin,
		BoundActor:              "worker",
		EventFeatures:           db.SupportedEventFeatures,
		AllowPartialNotEnrolled: true,
	}
	params := baseParams
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 10, Event: beforeEv},
		{SourceEventID: 11, Event: stuckEv},
		{SourceEventID: 12, Event: afterEv},
	}
	res, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 2, res.Accepted)
	require.Zero(t, res.Duplicates)
	require.EqualValues(t, 10, res.PushCursorEventID, "cursor stops before first refused event")
	require.Len(t, res.RefusedEvents, 1)
	require.Equal(t, int64(11), res.RefusedEvents[0].SourceEventID)
	require.Equal(t, stuckEv.EventUID, res.RefusedEvents[0].EventUID)
	require.Equal(t, db.FederationProjectNotEnrolledCode, res.RefusedEvents[0].Code)
	require.Equal(t, moved.UID, res.RefusedEvents[0].IssueUID)
	gotA, err := store.IssueByUID(ctx, stayingA.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Staying A updated", gotA.Title)
	gotB, err := store.IssueByUID(ctx, stayingB.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Staying B updated", gotB.Title)
	gotMoved, err := store.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, moved.Title, gotMoved.Title)

	// All-refused batch returns 0 accepted, 0 cursor, and lists every refused event when opted in,
	// and returns FederationProjectNotEnrolledError when AllowPartialNotEnrolled is false.
	stuck2 := newRemoteEvent(t, source, &moved.UID, "issue.updated", "worker", origin, at+4, jsontext.Value(`{"title":"Second stuck"}`))
	params = baseParams
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 20, Event: stuckEv},
		{SourceEventID: 21, Event: stuck2},
	}
	allRefused, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Zero(t, allRefused.Accepted)
	require.Zero(t, allRefused.Duplicates)
	require.Zero(t, allRefused.PushCursorEventID)
	require.Len(t, allRefused.RefusedEvents, 2)
	require.Equal(t, stuckEv.EventUID, allRefused.RefusedEvents[0].EventUID)
	require.Equal(t, stuck2.EventUID, allRefused.RefusedEvents[1].EventUID)

	strict := params
	strict.AllowPartialNotEnrolled = false
	_, err = store.IngestFederationEvents(ctx, strict)
	var notEnrolled *db.FederationProjectNotEnrolledError
	require.ErrorAs(t, err, &notEnrolled)
	require.Equal(t, moved.UID, notEnrolled.IssueUID)

	// In-batch duplicate of a refused event is refused (not counted as duplicate).
	okAfterDup := newRemoteEvent(t, source, &stayingA.UID, "issue.updated", "worker", origin, at+5, jsontext.Value(`{"title":"After dup"}`))
	params = baseParams
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 22, Event: stuckEv},
		{SourceEventID: 23, Event: stuckEv},
		{SourceEventID: 24, Event: okAfterDup},
	}
	dupRes, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 1, dupRes.Accepted)
	require.Zero(t, dupRes.Duplicates)
	require.Zero(t, dupRes.PushCursorEventID)
	require.Len(t, dupRes.RefusedEvents, 2)

	// Intra-batch causal dependency: creation linking to moved issue + follow-up edit on created issue.
	createdUID, err := uid.New()
	require.NoError(t, err)
	createdPayload, err := json.Marshal(map[string]any{
		"uid": createdUID, "short_id": "abcd", "title": "Created linking moved", "body": "",
		"author": "worker", "status": "open", "created_at": time.UnixMilli(at + 6).UTC().Format(time.RFC3339Nano),
		"metadata": map[string]any{},
		"links":    []map[string]any{{"type": "blocks", "to_issue_uid": moved.UID}},
	})
	require.NoError(t, err)
	createLinked := newRemoteEvent(t, source, &createdUID, "issue.created", "worker", origin, at+6, jsontext.Value(createdPayload))
	editCreated := newRemoteEvent(t, source, &createdUID, "issue.updated", "worker", origin, at+7, jsontext.Value(`{"title":"Follow-up edit"}`))
	unrelated := newRemoteEvent(t, source, &stayingB.UID, "issue.updated", "worker", origin, at+8, jsontext.Value(`{"title":"Unrelated after dep"}`))
	params = baseParams
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 30, Event: createLinked},
		{SourceEventID: 31, Event: editCreated},
		{SourceEventID: 32, Event: unrelated},
	}
	depRes, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 1, depRes.Accepted)
	require.Zero(t, depRes.PushCursorEventID)
	require.Len(t, depRes.RefusedEvents, 2)
	require.Equal(t, createLinked.EventUID, depRes.RefusedEvents[0].EventUID)
	require.Equal(t, editCreated.EventUID, depRes.RefusedEvents[1].EventUID)
	require.Equal(t, moved.UID, depRes.RefusedEvents[1].IssueUID)
	_, err = store.IssueByUID(ctx, createdUID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	gotB, err = store.IssueByUID(ctx, stayingB.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Unrelated after dep", gotB.Title)

	// Comment causal dependency: reply to comment on moved issue + edit of that reply.
	replyCommentUID, err := uid.New()
	require.NoError(t, err)
	replyPayload, err := json.Marshal(map[string]any{
		"comment_uid": replyCommentUID, "body": "reply to moved", "author": "worker",
		"reply_to_uid": movedComment.UID, "reply_kind": "reply",
	})
	require.NoError(t, err)
	replyEv := newRemoteEvent(t, source, &stayingA.UID, "issue.commented", "worker", origin, at+9, jsontext.Value(replyPayload))
	replyEv.RelatedIssueUID = &moved.UID
	replyEv.ContentHash, err = db.RemoteEventContentHash(replyEv)
	require.NoError(t, err)
	editReplyPayload, err := json.Marshal(map[string]any{"comment_uid": replyCommentUID, "body": "edited reply"})
	require.NoError(t, err)
	editReplyEv := newRemoteEvent(t, source, &stayingA.UID, "issue.comment_edited", "worker", origin, at+10, jsontext.Value(editReplyPayload))
	unrelated2 := newRemoteEvent(t, source, &stayingB.UID, "issue.updated", "worker", origin, at+11, jsontext.Value(`{"title":"Unrelated after reply"}`))
	params = baseParams
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 40, Event: replyEv},
		{SourceEventID: 41, Event: editReplyEv},
		{SourceEventID: 42, Event: unrelated2},
	}
	replyRes, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 1, replyRes.Accepted)
	require.Len(t, replyRes.RefusedEvents, 2)
	require.Equal(t, replyEv.EventUID, replyRes.RefusedEvents[0].EventUID)
	require.Equal(t, editReplyEv.EventUID, replyRes.RefusedEvents[1].EventUID)

	// Implicit displaced parent in unenrolled project: refuses only the displacing event
	// with empty IssueUID (preserving privacy), while accepting an unrelated sibling event.
	peerGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push", Actor: "peer-member",
	})
	require.NoError(t, err)
	childIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Child with hidden parent", Author: "worker"})
	require.NoError(t, err)
	_, _, err = store.CreateLinkAndEvent(ctx,
		db.CreateLinkParams{FromIssueID: childIssue.ID, ToIssueID: moved.ID, Type: "parent", Author: "worker"},
		db.LinkEventParams{EventType: "issue.linked", EventIssueID: childIssue.ID, FromUID: childIssue.UID, FromShortID: childIssue.ShortID, ToUID: moved.UID, ToShortID: moved.ShortID, Actor: "worker"},
	)
	require.NoError(t, err)
	newParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "New local parent", Author: "worker"})
	require.NoError(t, err)
	displacePayload, err := json.Marshal(map[string]string{"parent_set_uid": newParent.UID})
	require.NoError(t, err)
	displaceEv := newRemoteEvent(t, source, &childIssue.UID, "issue.links_changed", "worker", origin, at+11, jsontext.Value(displacePayload))
	unrelatedAfterDisplace := newRemoteEvent(t, source, &stayingB.UID, "issue.updated", "worker", origin, at+11, jsontext.Value(`{"title":"Unrelated after displace"}`))
	params = baseParams
	params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: peerGrant.Enrollment.ID}
	params.AuthorizeReference = func(_ context.Context, _ db.Transaction, _ db.FederationEnrollment, _ db.Project) error {
		return nil
	}
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 45, Event: displaceEv},
		{SourceEventID: 46, Event: unrelatedAfterDisplace},
	}
	displaceRes, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 1, displaceRes.Accepted)
	require.Zero(t, displaceRes.PushCursorEventID)
	require.Len(t, displaceRes.RefusedEvents, 1)
	require.Equal(t, displaceEv.EventUID, displaceRes.RefusedEvents[0].EventUID)
	require.Empty(t, displaceRes.RefusedEvents[0].IssueUID, "implicit displaced parent UID must not leak")
	parentLink, err := store.ParentOf(ctx, childIssue.ID)
	require.NoError(t, err)
	require.Equal(t, moved.ID, parentLink.ToIssueID)
	gotB, err = store.IssueByUID(ctx, stayingB.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Unrelated after displace", gotB.Title)

	// Hard error in the same batch still rolls back the entire batch.
	hostDenied := errors.New("host denied")
	otherTarget, err := store.CreateProject(ctx, "partial-host-denied")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, otherTarget.ID, "worker")
	require.NoError(t, err)
	otherIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Host denied issue", Author: "worker"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: otherIssue.ID, FromProjectID: source.ID, ToProjectID: otherTarget.ID,
		IfMatchRev: otherIssue.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	otherGrant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: origin, ProjectID: &otherTarget.ID, Capabilities: "push,pull", Actor: "worker",
	})
	require.NoError(t, err)
	beforeHard := newRemoteEvent(t, source, &stayingA.UID, "issue.updated", "worker", origin, at+12, jsontext.Value(`{"title":"Must roll back"}`))
	hardEv := newRemoteEvent(t, source, &otherIssue.UID, "issue.updated", "worker", origin, at+13, jsontext.Value(`{"title":"Host denied"}`))
	params = baseParams
	params.MoveSourceEnrollmentIDs = map[string]int64{otherTarget.UID: otherGrant.Enrollment.ID}
	params.AuthorizeMove = func(_ context.Context, _ db.Transaction, _ db.FederationEnrollment, project db.Project) error {
		if project.UID == otherTarget.UID {
			return hostDenied
		}
		return nil
	}
	params.Events = []db.FederationIngestEvent{
		{SourceEventID: 50, Event: beforeHard},
		{SourceEventID: 51, Event: stuckEv},
		{SourceEventID: 52, Event: hardEv},
	}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, hostDenied)
	gotA, err = store.IssueByUID(ctx, stayingA.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "After dup", gotA.Title, "hard error must roll back earlier events in the batch")

	// Spoke out-of-order acknowledgement folding.
	spokeProj, err := store.CreateProject(ctx, "partial-spoke-ack")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: spokeProj.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 99, HubProjectUID: spokeProj.UID, Enabled: true, PushEnabled: true, Actor: "worker",
	})
	require.NoError(t, err)
	_, e1, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spokeProj.ID, Title: "E1", Author: "worker"})
	require.NoError(t, err)
	_, e2, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spokeProj.ID, Title: "E2", Author: "worker"})
	require.NoError(t, err)
	_, e3, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spokeProj.ID, Title: "E3", Author: "worker"})
	require.NoError(t, err)

	require.NoError(t, store.AcknowledgeFederationPushEvents(ctx, spokeProj.ID, []string{e1.UID, e3.UID}))
	binding, err := store.FederationBindingByProject(ctx, spokeProj.ID)
	require.NoError(t, err)
	require.Equal(t, e1.ID, binding.PushCursorEventID, "cursor folds through contiguous prefix only")
	ackE3, err := store.IsFederationEventAcknowledged(ctx, spokeProj.ID, e3.UID)
	require.NoError(t, err)
	require.True(t, ackE3)
	ackE2, err := store.IsFederationEventAcknowledged(ctx, spokeProj.ID, e2.UID)
	require.NoError(t, err)
	require.False(t, ackE2)
	pending, err := store.PendingFederationPushEvents(ctx, spokeProj.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, e2.UID, pending[0].UID)
	count, _, err := store.PendingFederationPushStats(ctx, spokeProj.ID, store.InstanceUID(), binding.PushCursorEventID)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)

	require.NoError(t, store.AcknowledgeFederationPushEvents(ctx, spokeProj.ID, []string{e2.UID}))
	binding, err = store.FederationBindingByProject(ctx, spokeProj.ID)
	require.NoError(t, err)
	require.Equal(t, e3.ID, binding.PushCursorEventID, "cursor folds through previously acknowledged E3 once E2 resolves")
	pending, err = store.PendingFederationPushEvents(ctx, spokeProj.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Empty(t, pending)

	return nil
}
