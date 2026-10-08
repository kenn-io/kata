package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayCrossProjectBoundary exercises relay cross project boundary on the supplied native store.
// R6: new relay admission refuses existing cross-project links, and a shared
// project cannot serialize private endpoints via link or create-with-link.
func RunRelayCrossProjectBoundary(t *testing.T, store db.Storage, legacyCache ...func(context.Context, string, string) error) {
	ctx := t.Context()
	first, err := store.CreateProject(ctx, "first-project")
	require.NoError(t, err)
	second, err := store.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	shared, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	adopting, err := store.CreateProject(ctx, "adopting-project")
	require.NoError(t, err)
	adoptionIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: adopting.ID, Author: "member", Title: "Existing adoption link"})
	require.NoError(t, err)
	for _, p := range []db.Project{first, second, shared} {
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleHub, HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
		require.NoError(t, err)
	}
	old, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: first.ID, Author: "member", Title: "Existing legacy link"})
	require.NoError(t, err)
	private, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: second.ID, Author: "member", Title: "Private link endpoint"})
	require.NoError(t, err)
	adoptionLink, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: adoptionIssue.ID, ToIssueID: private.ID, Type: "blocks", Author: "member"})
	require.NoError(t, err)
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: adopting.ID, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: "00000000000000000000000010", Actor: "member", RelayProtocolVersion: db.RelayProtocolVersion})
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary, "negotiated adoption rejects before rewriting UID/history")
	unchanged, err := store.ProjectByID(ctx, adopting.ID)
	require.NoError(t, err)
	require.Equal(t, adopting.UID, unchanged.UID)
	preservedAdoptionLink, err := store.LinkByID(ctx, adoptionLink.ID)
	require.NoError(t, err)
	require.Equal(t, adoptionLink, preservedAdoptionLink)
	link, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: old.ID, ToIssueID: private.ID, Type: "blocks", Author: "member"})
	require.NoError(t, err, "existing direct-hub link contract remains available")
	public, signingKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	for _, p := range []db.Project{first, shared} {
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	}
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: "boundary-parent-test-token"})
	require.NoError(t, err)
	params := db.CreateRelayEnrollmentParams{ProjectID: first.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: 1, Token: "boundary-rejected-test-token"}
	_, err = store.CreateRelayEnrollment(db.WithAuthorizedProjects(ctx, []string{}), params)
	require.ErrorIs(t, err, db.ErrNotFound, "hidden admission cannot reveal whether a boundary exists")
	_, err = store.CreateRelayEnrollment(ctx, params)
	require.Error(t, err, "admission cannot silently strip an existing hashed cross-project link")
	preserved, err := store.LinkByID(ctx, link.ID)
	require.NoError(t, err)
	require.Equal(t, link, preserved)
	_, err = store.AuthorizeFederationToken(ctx, params.Token, first.ID, "pull")
	require.ErrorIs(t, err, db.ErrNotFound)
	unlinkEvent, err := store.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: old.ID, Actor: "member", FromUID: old.UID, ToUID: private.UID, FromShortID: old.ShortID, ToShortID: private.ShortID})
	require.NoError(t, err)
	admitted, err := store.CreateRelayEnrollment(ctx, params)
	require.NoError(t, err, "explicitly removing the boundary allows enrollment")
	bootstrap, ok := store.(db.RelayResetBootstrapStore)
	require.True(t, ok)
	reset, err := bootstrap.RelayEnrollmentNeedsReset(ctx, admitted.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.True(t, reset, "historical private endpoints require a signed current-state bootstrap")
	history, err := store.PendingRelayDeliveries(ctx, admitted.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1024)
	require.NoError(t, err)
	require.Empty(t, history, "private historical endpoints cannot leave in original event bodies")
	resets, ok := store.(db.RelayResetStore)
	require.True(t, ok)
	checkpoint, err := resets.CreateRelayReset(ctx, admitted.Enrollment.RelayBindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: signingKey})
	require.NoError(t, err)
	if len(legacyCache) > 0 {
		// Model an authentic prior-builder checkpoint: the original source bytes
		// contained a private historical endpoint and history_event_id was absent.
		var sources [][]byte
		require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Events, &sources))
		historical, err := db.EncodeRelaySourceEvent(remoteEventFromStored(unlinkEvent))
		require.NoError(t, err)
		sources = append(sources, historical)
		checkpoint.Snapshot.Events, err = json.Marshal(sources)
		require.NoError(t, err)
		checkpoint.Translation.SnapshotDigest, err = db.RootResetSnapshotDigest(checkpoint.Snapshot)
		require.NoError(t, err)
		checkpoint.Manifest.HistoryEventID = 0
		checkpoint.Manifest, err = db.SignRootResetManifest(checkpoint.Manifest, checkpoint.Snapshot, signingKey)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootResetManifest(db.RootKeyPin{ProjectUID: first.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}, checkpoint.Manifest, checkpoint.Snapshot))
		raw, err := json.Marshal(checkpoint)
		require.NoError(t, err)
		key := db.RelayResetMetadataPrefix + first.UID + "." + admitted.Enrollment.RelayBindingUID
		require.NoError(t, legacyCache[0](ctx, key, string(raw)))
		checkpoint, err = resets.CreateRelayReset(ctx, admitted.Enrollment.RelayBindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: signingKey})
		require.NoError(t, err)
	}
	for _, section := range [][]byte{checkpoint.Snapshot.Events, checkpoint.Snapshot.Entities} {
		var bodies [][]byte
		require.NoError(t, json.Unmarshal(section, &bodies))
		for _, body := range bodies {
			source, err := db.DecodeRelaySourceEvent(body)
			require.NoError(t, err)
			require.NotContains(t, string(source.Payload), private.UID, "signed reset cannot disclose an excluded historical endpoint")
			if source.RelatedIssueUID != nil {
				require.NotEqual(t, private.UID, *source.RelatedIssueUID)
			}
		}
	}
	params.ProjectID = shared.ID
	params.Token = "boundary-shared-test-token"
	_, err = store.CreateRelayEnrollment(ctx, params)
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: shared.ID, Author: "member", Title: "Shared issue"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: private.ID, Type: "blocks", Author: "member"})
	require.Error(t, err, "new link cannot cross relay boundary")
	atomicTitle := "must roll back with rejected edge"
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "member", Title: &atomicTitle,
		AddBlocks: []int64{private.ID},
	})
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary,
		"atomic block insertion cannot bypass the negotiated relay boundary")
	unchangedIssue, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, "Shared issue", unchangedIssue.Title, "the scalar edit rolls back with the rejected edge")
	sharedLinks, err := store.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Empty(t, sharedLinks)
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "member", SetParent: &private.ID,
	})
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary,
		"atomic parent insertion cannot bypass the negotiated relay boundary")
	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: private.ID, ToIssueID: issue.ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: private.ID, Actor: "member"})
	require.Error(t, err, "opposite endpoint and event route cannot bypass relay boundary")
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: shared.ID, Author: "member", Title: "Rejected initial private link", Links: []db.InitialLink{{Type: "blocks", ToNumber: private.ID}}})
	require.Error(t, err)
	local, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: shared.ID, Author: "member", Title: "Allowed same-project link", Links: []db.InitialLink{{Type: "blocks", ToNumber: issue.ID}}})
	require.NoError(t, err)
	links, err := store.LinksByIssue(ctx, local.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	oldParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: shared.ID, Author: "member", Title: "Existing shared parent"})
	require.NoError(t, err)
	oldParentLink, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: local.ID, ToIssueID: oldParent.ID, Type: "parent", Author: "member",
	})
	require.NoError(t, err)
	replacementStore, ok := store.(db.ParentLinkReplacementStorage)
	require.True(t, ok, "native stores support atomic parent replacement")
	newParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: shared.ID, Author: "member", Title: "Replacement shared parent"})
	require.NoError(t, err)
	replacement, err := replacementStore.ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
		ExpectedParentLinkID: oldParentLink.ID, ExpectedParentIssueID: oldParent.ID,
		Link: db.CreateLinkParams{
			FromIssueID: local.ID, ToIssueID: newParent.ID, Type: "parent", Author: "member",
		},
		UnlinkEvent: db.LinkEventParams{
			EventType: "issue.unlinked", EventIssueID: local.ID,
			FromShortID: local.ShortID, FromUID: local.UID,
			ToShortID: oldParent.ShortID, ToUID: oldParent.UID, Actor: "member",
		},
		LinkEvent: db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: local.ID,
			FromShortID: local.ShortID, FromUID: local.UID,
			ToShortID: newParent.ShortID, ToUID: newParent.UID, Actor: "member",
		},
	})
	require.NoError(t, err, "same-project parent replacement commits")
	require.Equal(t, "issue.unlinked", replacement.UnlinkedEvent.Type)
	require.Equal(t, "issue.linked", replacement.LinkedEvent.Type)
	require.Equal(t, newParent.ID, replacement.Link.ToIssueID)
	currentParentLink := replacement.Link
	eventsBeforeReplace, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: shared.ID, Limit: 1000})
	require.NoError(t, err)
	require.NotEmpty(t, eventsBeforeReplace)
	_, err = replacementStore.ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
		ExpectedParentLinkID: currentParentLink.ID, ExpectedParentIssueID: newParent.ID,
		Link: db.CreateLinkParams{
			FromIssueID: local.ID, ToIssueID: private.ID, Type: "parent", Author: "member",
		},
		UnlinkEvent: db.LinkEventParams{
			EventType: "issue.unlinked", EventIssueID: local.ID,
			FromShortID: local.ShortID, FromUID: local.UID,
			ToShortID: newParent.ShortID, ToUID: newParent.UID, Actor: "member",
		},
		LinkEvent: db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: local.ID,
			FromShortID: local.ShortID, FromUID: local.UID,
			ToShortID: private.ShortID, ToUID: private.UID, Actor: "member",
		},
	})
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary,
		"atomic parent replacement cannot bypass the negotiated relay boundary")
	parentAfterReject, err := store.ParentOf(ctx, local.ID)
	require.NoError(t, err)
	require.Equal(t, currentParentLink, parentAfterReject, "rejected replacement preserves the current parent")
	eventsAfterReplace, err := store.EventsAfter(ctx, db.EventsAfterParams{
		AfterID: eventsBeforeReplace[len(eventsBeforeReplace)-1].ID, ProjectID: shared.ID, Limit: 10,
	})
	require.NoError(t, err)
	require.Empty(t, eventsAfterReplace, "rejected replacement emits no unlink or link event")
}
