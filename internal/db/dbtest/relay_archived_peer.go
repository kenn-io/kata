package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkRelayArchivedSourceRestoration(t *testing.T, root db.Storage, backend Backend) error {
	ctx := t.Context()
	fixture := NewRelaySharedAccessFixture(t, root)
	replica := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, replica.Close()) })
	features := db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures)
	peerBinding, peerEpoch := setupRelayHop(t, root, replica, fixture.Projects[1], "archive-peer", false)
	sourceBinding, sourceEpoch := setupRelayHop(t, root, replica, fixture.Projects[0], "archive-source", false)
	syncRelayHop(features, t, root, replica, peerBinding, peerEpoch, db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact)
	syncRelayHop(features, t, root, replica, sourceBinding, sourceEpoch, db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact)
	source, err := replica.ProjectByUID(ctx, fixture.Projects[0].UID)
	require.NoError(t, err)
	query := db.UISnapshotQuery{SelectedIssueUID: fixture.Issues[0].UID}
	ui := replica.(db.UIStore)
	snapshot, err := ui.ReadUISnapshot(ctx, query)
	require.NoError(t, err)
	require.Len(t, snapshot.SelectedLinks, 1)
	_, _, err = replica.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "member", Force: true})
	require.NoError(t, err)
	_, _, err = root.CreateIssue(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateIssueParams{ProjectID: fixture.Projects[1].ID, Author: "member", Title: "Peer update"})
	require.NoError(t, err)
	syncRelayHop(features, t, root, replica, peerBinding, peerEpoch, db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact)
	_, _, changed, err := replica.RestoreProject(ctx, source.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	snapshot, err = ui.ReadUISnapshot(ctx, query)
	require.NoError(t, err)
	require.Len(t, snapshot.SelectedLinks, 1)
	require.Equal(t, fixture.Issues[1].UID, snapshot.SelectedLinks[0].ToIssueUID)
	peer, err := replica.ProjectByUID(ctx, fixture.Projects[1].UID)
	require.NoError(t, err)
	_, _, err = replica.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: peer.ID, Actor: "member", Force: true})
	require.NoError(t, err)
	snapshot, err = ui.ReadUISnapshot(ctx, query)
	require.NoError(t, err)
	require.Empty(t, snapshot.SelectedLinks)
	other, _, err := root.CreateIssue(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateIssueParams{ProjectID: fixture.Projects[0].ID, Author: "member", Title: "Other issue"})
	require.NoError(t, err)
	_, _, err = root.CreateLinkAndEvent(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateLinkParams{FromIssueID: fixture.Issues[0].ID, ToIssueID: other.ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: fixture.Issues[0].ID, Actor: "member", FromUID: fixture.Issues[0].UID, ToUID: other.UID, FromShortID: fixture.Issues[0].ShortID, ToShortID: other.ShortID})
	require.NoError(t, err)
	newIssue, _, err := root.CreateIssue(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateIssueParams{ProjectID: fixture.Projects[0].ID, Author: "member", Title: "After archive"})
	require.NoError(t, err)
	_, _, err = root.CreateLinkAndEvent(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateLinkParams{FromIssueID: newIssue.ID, ToIssueID: fixture.Issues[1].ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: newIssue.ID, Actor: "member", FromUID: newIssue.UID, ToUID: fixture.Issues[1].UID, FromShortID: newIssue.ShortID, ToShortID: fixture.Issues[1].ShortID})
	require.NoError(t, err)
	syncRelayHop(features, t, root, replica, sourceBinding, sourceEpoch, db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact)
	binding, err := replica.FederationBindingByProject(ctx, source.ID)
	require.NoError(t, err)
	restored := *binding.RelayConfig
	revoked := restored
	revoked.UpstreamRevoked = true
	binding, err = replica.SetRelayBindingConfig(ctx, source.ID, revoked, restored)
	require.NoError(t, err)
	require.True(t, binding.RelayConfig.UpstreamRevoked)
	binding, err = replica.SetRelayBindingConfig(ctx, source.ID, restored, revoked)
	require.NoError(t, err)
	require.False(t, binding.RelayConfig.UpstreamRevoked)
	_, _, changed, err = replica.RestoreProject(ctx, peer.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	received, err := replica.IssueByUID(ctx, newIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, []string{fixture.Issues[1].UID}, linkTargetUIDs(t, replica, received.ID))
	snapshot, err = ui.ReadUISnapshot(ctx, query)
	require.NoError(t, err)
	require.Len(t, snapshot.SelectedLinks, 2)
	require.ElementsMatch(t, []string{fixture.Issues[1].UID, other.UID}, []string{snapshot.SelectedLinks[0].ToIssueUID, snapshot.SelectedLinks[1].ToIssueUID})
	return nil
}

func checkRelayArchivedPeerReconnect(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	fixture := NewRelaySharedAccessFixture(t, store)
	grant := fixture.Grant
	features := db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures)
	syncRelayHop(features, t, store, nil, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch, db.RelayStreamEvent)
	before := relayStateForBinding(t, store, grant.Enrollment.RelayBindingUID)
	_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: fixture.Projects[1].ID, Actor: "member", Force: true})
	require.NoError(t, err)
	_, _, err = store.RevokeAPIToken(ctx, *grant.Enrollment.ParentTokenID, "admin")
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", TeamUIDs: []string{fixture.Team.UID}, PlaintextToken: "replacement-parent-test-token"})
	require.NoError(t, err)
	t.Run("reconnect check", func(t *testing.T) {
		require.NoError(t, store.CheckRelayProjectLinks(ctx, fixture.Projects[0].ID))
	})
	t.Run("credential rebind", func(t *testing.T) {
		rebound, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.Projects[0].ID, ParentTokenID: parent.ID, SpokeInstanceUID: grant.Enrollment.SpokeInstanceUID, ProtocolVersion: db.RelayProtocolVersion, Token: grant.Token, RebindParent: true})
		require.NoError(t, err)
		require.Equal(t, grant.Enrollment.RelayBindingUID, rebound.Enrollment.RelayBindingUID)
		require.Equal(t, grant.Enrollment.RelayResetEpoch, rebound.Enrollment.RelayResetEpoch)
		require.Equal(t, before, relayStateForBinding(t, store, grant.Enrollment.RelayBindingUID))
		_, err = store.AuthorizeFederationToken(ctx, grant.Token, fixture.Projects[0].ID, "pull")
		require.NoError(t, err)
	})
	return nil
}
