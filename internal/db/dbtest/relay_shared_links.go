package dbtest

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RelaySharedAccessFixture is one native root and its common-audience pair.
type RelaySharedAccessFixture struct {
	Projects [2]db.Project
	Issues   [2]db.Issue
	Grant    db.CreatedFederationEnrollment
	Team     db.Team
	Signer   db.RootAttributionSigner
}

// RunRelayEventReleasePeerAccess checks access before offering events.
func RunRelayEventReleasePeerAccess(t *testing.T, store db.Storage, backend Backend) {
	ctx := t.Context()
	fixture := NewRelaySharedAccessFixture(t, store)
	features := db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures)
	denied := errors.New("peer denied")
	rejectPeer := db.WithProjectAccessTransactionCheck(features, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	_, _, err := store.CreateLinkAndEvent(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateLinkParams{FromIssueID: fixture.Issues[0].ID, ToIssueID: fixture.Issues[1].ID, Type: "parent", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: fixture.Issues[0].ID, Actor: "member", FromUID: fixture.Issues[0].UID, ToUID: fixture.Issues[1].UID})
	require.NoError(t, err)
	binding := fixture.Grant.Enrollment.RelayBindingUID
	before := relayStateForBinding(t, store, binding)
	out, err := store.PendingRelayDeliveries(rejectPeer, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
	checks := 0
	countPeer := db.WithProjectAccessTransactionCheck(features, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			checks++
		}
		return nil
	})
	offered, err := store.PendingRelayDeliveries(countPeer, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, 1, checks)
	require.NotEmpty(t, offered)
	retry, err := store.PendingRelayDeliveries(features, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, retry)
	last := offered[len(offered)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, last.Sequence, last.Digest))
	peerComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: fixture.Issues[1].ID, Author: "member", Body: "Peer comment"})
	require.NoError(t, err)
	_, reply, err := store.CreateComment(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateCommentParams{IssueID: fixture.Issues[0].ID, Author: "member", Body: "Cross-project reply", ReplyToUID: peerComment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	rejectOlder := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	_, err = store.PendingRelayDeliveries(rejectOlder, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	_, ordinary, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.Projects[0].ID, Author: "member", Title: "Before crossing"})
	require.NoError(t, err)
	_, crossing, err := store.CreateLinkAndEvent(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.CreateLinkParams{FromIssueID: fixture.Issues[0].ID, ToIssueID: fixture.Issues[1].ID, Type: "related", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: fixture.Issues[0].ID, Actor: "member", FromUID: fixture.Issues[0].UID, ToUID: fixture.Issues[1].UID})
	require.NoError(t, err)
	checks = 0
	olderPeer := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			checks++
		}
		return nil
	})
	prefix, err := store.PendingRelayDeliveries(olderPeer, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, 1, checks)
	require.Len(t, prefix, 2)
	require.Equal(t, reply.UID, prefix[0].SourceUID)
	require.Equal(t, ordinary.UID, prefix[1].SourceUID)
	replayed, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, prefix, replayed)
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, prefix[1].Sequence, prefix[1].Digest))
	_, err = store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	next, err := store.PendingRelayDeliveries(features, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, crossing.UID, next[0].SourceUID)
	require.Equal(t, crossing.ContentHash, next[0].SourceHash)
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, next[0].Sequence, next[0].Digest))
	// The hub has not received the linked peer project.
	hub := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hub.Close()) })
	upstream, upstreamEpoch := setupRelayHop(t, store, hub, fixture.Projects[0], "unresolved-hub", true)
	syncRelayHop(features, t, store, hub, upstream, upstreamEpoch, db.RelayStreamEvent, db.RelayStreamReceipt)
	hubProject, err := hub.ProjectByUID(ctx, fixture.Projects[0].UID)
	require.NoError(t, err)
	policy, err := hub.ProjectAccessPolicy(ctx, hubProject.UID)
	require.NoError(t, err)
	parent, _, err := hub.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", TeamUIDs: policy.TeamUIDs, PlaintextToken: "unresolved-leaf-parent-test-token"})
	require.NoError(t, err)
	grant, err := hub.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: hubProject.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "unresolved-leaf-relay-test-token"})
	require.NoError(t, err)
	downstream, epoch := grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch

	// Earlier ordinary events may be offered as a prefix. The link must stop
	// an old peer before the missing endpoint's project reaches this hub.
	for {
		items, err := hub.PendingRelayDeliveries(ctx, downstream, db.RelayStreamEvent, 100)
		if err != nil {
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			break
		}
		require.NotEmpty(t, items)
		last := items[len(items)-1]
		require.NoError(t, hub.AckRelayDeliveries(ctx, downstream, epoch, db.RelayStreamEvent, last.Sequence, last.Digest))
	}
	items, err := hub.PendingRelayDeliveries(features, downstream, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	found := false
	for _, item := range items {
		if item.SourceUID == crossing.UID {
			found = true
		}
	}
	require.True(t, found)
}

// NewRelaySharedAccessFixture enrolls the same native pair for storage and
// relay tests, keeping the initial regression at native admission.
func NewRelaySharedAccessFixture(t *testing.T, store db.Storage) RelaySharedAccessFixture {
	t.Helper()

	ctx := t.Context()
	team, _, err := store.CreateTeam(ctx, "shared-audience", "admin")
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "member", AdminActor: "admin", TeamUIDs: []string{team.UID},
		PlaintextToken: "shared-audience-parent-test-token",
	})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var projects [2]db.Project
	var issues [2]db.Issue
	for i, name := range []string{"first-shared-project", "second-shared-project"} {
		projects[i], err = store.CreateProject(ctx, name)
		require.NoError(t, err)
		project := projects[i]
		policy, err := store.ProjectAccessPolicy(ctx, project.UID)
		require.NoError(t, err)
		policy.Visibility, policy.TeamUIDs = "teams", []string{team.UID}
		_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID,
			Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID,
			AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		issues[i], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID,
			Author: "member", Title: "Shared endpoint"})
		require.NoError(t, err)
	}
	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issues[0].ID,
		ToIssueID: issues[1].ID, Type: "blocks", Author: "member"},
		db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "member",
			FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: projects[0].ID,
		ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006",
		ProtocolVersion: db.RelayProtocolVersion, Token: "first-shared-relay-test-token"})
	require.NoError(t, err)

	return RelaySharedAccessFixture{Projects: projects, Issues: issues, Grant: grant, Team: team, Signer: db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}}
}
