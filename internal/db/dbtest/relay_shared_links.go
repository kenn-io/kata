package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelaySharedLinks checks shared links across relay deliveries.
func RunRelaySharedLinks(t *testing.T, store db.Storage) {
	ctx := t.Context()
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: "shared-audience-parent-test-token"})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var projects [2]db.Project
	var issues [2]db.Issue
	for i, name := range []string{"first-shared-project", "second-shared-project"} {
		projects[i], err = store.CreateProject(ctx, name)
		require.NoError(t, err)
		project := projects[i]
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		issues[i], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Shared endpoint"})
		require.NoError(t, err)
	}
	_, linked, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "member", FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: projects[0].ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "first-shared-relay-test-token"})
	require.NoError(t, err)
	binding := grant.Enrollment.RelayBindingUID
	offered, err := store.PendingRelayDeliveries(db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures), binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, offered)
	var crossing db.RelayEnvelope
	for _, envelope := range offered {
		if envelope.SourceUID == linked.UID {
			crossing = envelope
		}
	}
	require.Equal(t, linked.ContentHash, crossing.SourceHash)
	last := offered[len(offered)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, last.Sequence, last.Digest))
	_, ordinary, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Author: "member", Title: "Before crossing"})
	require.NoError(t, err)
	_, crossingEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "related", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "member", FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
	require.NoError(t, err)
	prefix, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Len(t, prefix, 1)
	require.Equal(t, ordinary.UID, prefix[0].SourceUID)
	replayed, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, prefix, replayed)
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, prefix[0].Sequence, prefix[0].Digest))
	_, err = store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	next, err := store.PendingRelayDeliveries(db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures), binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, crossingEvent.UID, next[0].SourceUID)
	require.Equal(t, crossingEvent.ContentHash, next[0].SourceHash)
}
