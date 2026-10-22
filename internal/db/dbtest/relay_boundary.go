package dbtest

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayCrossProjectBoundary exercises relay cross project boundary on the supplied native store.
// R6: new relay admission refuses existing cross-project links, and a shared
// project cannot serialize private endpoints via link or create-with-link.
func RunRelayCrossProjectBoundary(t *testing.T, store db.Storage) {
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
	parentLink, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: old.ID, ToIssueID: private.ID, Type: "parent", Author: "member"})
	require.NoError(t, err)
	_, closeEvents, changed, err := store.CloseIssueWithEvents(ctx, old.ID, "done", "member", "closed with a parent", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, closeEvents, 1)
	require.Contains(t, closeEvents[0].Payload, private.UID,
		"the close event must capture the parent UID before its link is removed")
	require.Contains(t, closeEvents[0].Payload, private.ShortID)
	public, signingKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	for _, p := range []db.Project{first, shared} {
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	}
	// An existing legacy spoke can also have local cross-project links. The
	// negotiated setup path must return the same boundary sentinel without
	// partially installing a relay configuration.
	rootUID := "00000000000000000000000002"
	var legacyProjects [2]db.Project
	var legacyIssues [2]db.Issue
	for i, name := range []string{"legacy-project", "legacy-peer-project"} {
		project, err := store.CreateProject(ctx, name)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: int64(42 + i), HubProjectUID: project.UID, Actor: "member", PushEnabled: true, Enabled: true})
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Legacy linked issue", Author: "member"})
		require.NoError(t, err)
		legacyProjects[i] = project
		legacyIssues[i] = issue
	}
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: legacyIssues[0].ID, ToIssueID: legacyIssues[1].ID, Type: "blocks", Author: "member"})
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: legacyProjects[0].UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	_, err = store.SetRelayBindingConfig(ctx, legacyProjects[0].ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000012", UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "member", ResetEpoch: 1})
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
	legacy, err := store.FederationBindingByProject(ctx, legacyProjects[0].ID)
	require.NoError(t, err)
	require.Nil(t, legacy.RelayConfig)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: "boundary-parent-test-token"})
	require.NoError(t, err)
	require.ErrorIs(t, store.CheckRelayProjectLinks(ctx, first.ID), db.ErrFederationCrossProjectBoundary)
	require.ErrorIs(t, store.CheckRelayProjectLinks(ctx, second.ID), db.ErrFederationCrossProjectBoundary, "incoming links count too")
	require.NoError(t, store.CheckRelayProjectLinks(ctx, shared.ID))
	require.ErrorIs(t, store.CheckRelayProjectLinks(db.WithAuthorizedProjects(ctx, []string{}), first.ID), db.ErrNotFound)
	params := db.CreateRelayEnrollmentParams{ProjectID: first.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: 1, Token: "boundary-rejected-test-token"}
	_, err = store.CreateRelayEnrollment(db.WithAuthorizedProjects(ctx, []string{}), params)
	require.ErrorIs(t, err, db.ErrNotFound, "hidden admission cannot reveal whether a boundary exists")
	_, err = store.CreateRelayEnrollment(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary, "admission cannot silently strip an existing hashed cross-project link")
	preserved, err := store.LinkByID(ctx, link.ID)
	require.NoError(t, err)
	require.Equal(t, link, preserved)
	_, err = store.AuthorizeFederationToken(ctx, params.Token, first.ID, "pull")
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.DeleteLinkAndEvent(ctx, parentLink, db.LinkEventParams{
		EventType: "issue.unlinked", EventIssueID: old.ID, Actor: "member",
		FromUID: old.UID, ToUID: private.UID, FromShortID: old.ShortID, ToShortID: private.ShortID,
	})
	require.NoError(t, err)
	_, err = store.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: old.ID, Actor: "member", FromUID: old.UID, ToUID: private.UID, FromShortID: old.ShortID, ToShortID: private.ShortID})
	require.NoError(t, err)
	require.NoError(t, store.CheckRelayProjectLinks(ctx, first.ID), "historical links do not block admission after unlinking")
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
	requireResetOmitsIssue(t, checkpoint, private)
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

	// Closing evidence can outlive the issue's original project. A later relay
	// reset must omit the entire historical close when its target has moved out
	// of the selected project, for both UID and qualified short-ID references.
	evidenceProject, err := store.CreateProject(ctx, "evidence-shared-project")
	require.NoError(t, err)
	restrictedProject, err := store.CreateProject(ctx, "restricted-project")
	require.NoError(t, err)
	uidTarget, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: evidenceProject.ID, Author: "member", Title: "UID evidence target"})
	require.NoError(t, err)
	// This short ID also occurs in the JSON field name created_at.
	shortTarget, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: evidenceProject.ID, UID: "0000000000000000000000CREA", Author: "member", Title: "Short ID evidence target"})
	require.NoError(t, err)
	require.Equal(t, "crea", shortTarget.ShortID)
	uidSource, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: evidenceProject.ID, Author: "member", Title: "UID evidence source"})
	require.NoError(t, err)
	shortSource, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: evidenceProject.ID, Author: "member", Title: "Short ID evidence source"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: uidTarget.ID, FromProjectID: evidenceProject.ID, ToProjectID: restrictedProject.ID,
		IfMatchRev: uidTarget.Revision, Actor: "member",
	})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: shortTarget.ID, FromProjectID: evidenceProject.ID, ToProjectID: restrictedProject.ID,
		IfMatchRev: shortTarget.Revision, Actor: "member",
	})
	require.NoError(t, err)
	_, uidCloseEvents, changed, err := store.CloseIssueWithEvents(ctx, uidSource.ID, "done", "member", "closed with duplicate evidence", []db.Evidence{{Type: "duplicate-of", IssueRef: uidTarget.UID}})
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, uidCloseEvents, 1)
	require.Contains(t, uidCloseEvents[0].Payload, uidTarget.UID)
	_, shortCloseEvents, changed, err := store.CloseIssueWithEvents(ctx, shortSource.ID, "done", "member", "closed with superseded evidence", []db.Evidence{{Type: "superseded-by", IssueRef: restrictedProject.Name + "#" + shortTarget.ShortID}})
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, shortCloseEvents, 1)
	require.Contains(t, shortCloseEvents[0].Payload, restrictedProject.Name+"#"+shortTarget.ShortID)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: evidenceProject.ID, Role: db.FederationRoleHub, HubProjectID: evidenceProject.ID, HubProjectUID: evidenceProject.UID, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: evidenceProject.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	evidenceGrant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: evidenceProject.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000011",
		ProtocolVersion: db.RelayProtocolVersion, Token: "boundary-evidence-test-token", // #nosec G101 -- synthetic relay token used only in this test.
	})
	require.NoError(t, err)
	evidenceReset, ok := store.(db.RelayResetBootstrapStore)
	require.True(t, ok)
	resetRequired, err := evidenceReset.RelayEnrollmentNeedsReset(ctx, evidenceGrant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.True(t, resetRequired, "foreign close evidence requires a safe current-state reset")
	evidenceResets, ok := store.(db.RelayResetStore)
	require.True(t, ok)
	evidenceCheckpoint, err := evidenceResets.CreateRelayReset(ctx, evidenceGrant.Enrollment.RelayBindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: signingKey})
	require.NoError(t, err)
	for _, section := range [][]byte{evidenceCheckpoint.Snapshot.Events, evidenceCheckpoint.Snapshot.Entities} {
		var bodies [][]byte
		require.NoError(t, json.Unmarshal(section, &bodies))
		for _, body := range bodies {
			source, err := db.DecodeRelaySourceEvent(body)
			require.NoError(t, err)
			require.NotContains(t, string(source.Payload), uidTarget.UID, "reset cannot disclose a UID evidence target from another project")
			require.NotContains(t, string(source.Payload), shortTarget.UID, "reset cannot disclose a short-ID evidence target from another project")
			var payload struct {
				Evidence []db.Evidence `json:"evidence"`
			}
			require.NoError(t, json.Unmarshal(source.Payload, &payload))
			for _, evidence := range payload.Evidence {
				require.NotEqual(t, shortTarget.ShortID, evidence.IssueRef, "reset cannot disclose a short-ID evidence reference from another project")
				require.NotEqual(t, restrictedProject.Name+"#"+shortTarget.ShortID, evidence.IssueRef, "reset cannot disclose a qualified evidence reference from another project")
			}
		}
	}
}
