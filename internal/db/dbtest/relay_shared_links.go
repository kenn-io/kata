package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RelaySharedAccessFixture is one native root and its common-audience pair.
type RelaySharedAccessFixture struct {
	Projects [2]db.Project
	Issues   [2]db.Issue
	Grants   [2]db.CreatedFederationEnrollment
	Linked   db.Event
	Team     db.Team
	Signer   db.RootAttributionSigner
}

// NewRelaySharedAccessFixture enrolls the same native pair for storage and
// embedding-boundary tests, keeping the initial regression at native admission.
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
	_, linked, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issues[0].ID,
		ToIssueID: issues[1].ID, Type: "blocks", Author: "member"},
		db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "member",
			FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
	require.NoError(t, err)
	var grants [2]db.CreatedFederationEnrollment
	for i, project := range projects {
		grants[i], err = store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID,
			ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006",
			ProtocolVersion: db.RelayProtocolVersion, Token: []string{"first-shared-relay-test-token", "second-shared-relay-test-token"}[i]})
		require.NoError(t, err, "root-authored crossing links with a shared audience can enroll")
	}

	return RelaySharedAccessFixture{Projects: projects, Issues: issues, Grants: grants, Linked: linked, Team: team, Signer: db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}}
}

// RunRelaySharedLinks checks shared links across relay deliveries.
func RunRelaySharedLinks(t *testing.T, store db.Storage) {
	fixture := NewRelaySharedAccessFixture(t, store)
	ctx := t.Context()
	binding := fixture.Grants[0].Enrollment.RelayBindingUID
	initial, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, initial)
	offered, err := store.PendingRelayDeliveries(db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures), binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	var crossing db.RelayEnvelope
	for _, envelope := range offered {
		if envelope.SourceUID == fixture.Linked.UID {
			crossing = envelope
		}
	}
	require.Equal(t, fixture.Linked.ContentHash, crossing.SourceHash)
	retry, err := store.PendingRelayDeliveries(db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures), binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, retry)
	last := offered[len(offered)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, last.Sequence, last.Digest))
	_, ordinary, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.Projects[0].ID, Author: "member", Title: "Before crossing"})
	require.NoError(t, err)
	_, crossingEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: fixture.Issues[0].ID, ToIssueID: fixture.Issues[1].ID, Type: "related", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: fixture.Issues[0].ID, Actor: "member", FromUID: fixture.Issues[0].UID, ToUID: fixture.Issues[1].UID, FromShortID: fixture.Issues[0].ShortID, ToShortID: fixture.Issues[1].ShortID})
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
}
