package dbtest

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayEnrollmentScope exercises relay enrollment scope on the supplied native store.
// R3: the existing account credential enrolls one project and device. Labels
// cannot select the accountable actor, and current parent/team authority is
// required at both transport admission and domain commit.
func RunRelayEnrollmentScope(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	privateProject, err := store.CreateProject(ctx, "private-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(pub), PublicKey: pub}))
	team, _, err := store.CreateTeam(ctx, "project-team", "admin")
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "parent-test-token", Actor: "member", AdminActor: "admin", TeamUIDs: []string{team.UID}})
	require.NoError(t, err)
	policy, err := store.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	policy.Visibility = "teams"
	policy.TeamUIDs = []string{team.UID}
	_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	p := db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "narrow-relay-test-token"}
	created, err := store.CreateRelayEnrollment(ctx, p)
	require.NoError(t, err)
	require.Equal(t, "member", created.Enrollment.Actor)
	require.Equal(t, &project.ID, created.Enrollment.ProjectID)
	require.Equal(t, &parent.ID, created.Enrollment.ParentTokenID)
	require.True(t, uid.Valid(created.Enrollment.RelayBindingUID))
	require.Equal(t, db.RelayProtocolVersion, created.Enrollment.RelayProtocolVersion)
	grant, err := store.AuthorizeFederationToken(ctx, created.Token, project.ID, "pull")
	require.NoError(t, err)
	require.Equal(t, created.Enrollment, grant)
	_, err = store.AuthorizeFederationToken(ctx, created.Token, privateProject.ID, "pull")
	require.ErrorIs(t, err, db.ErrNotFound)
	replay, err := store.CreateRelayEnrollment(ctx, p)
	require.NoError(t, err)
	require.Equal(t, created, replay, "lost response retains the same enrolled binding and secret")
	for _, change := range []func(*db.CreateRelayEnrollmentParams){
		func(p *db.CreateRelayEnrollmentParams) { p.ProjectID = 0 },
		func(p *db.CreateRelayEnrollmentParams) { p.Actor = "outsider" },
		func(p *db.CreateRelayEnrollmentParams) { p.ProtocolVersion++ },
		func(p *db.CreateRelayEnrollmentParams) { p.SpokeInstanceUID = store.InstanceUID() },
		func(p *db.CreateRelayEnrollmentParams) { p.ProjectID = privateProject.ID },
	} {
		wrong := p
		change(&wrong)
		wrong.Token = "invalid-relay-test-token"
		_, err = store.CreateRelayEnrollment(ctx, wrong)
		require.Error(t, err)
	}
	count, err := store.CountActiveFederationEnrollments(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "unsupported or unselected peers leave no enrollment")
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Subtree cannot enroll a bridge", Author: "member"})
	require.NoError(t, err)
	expires := time.Now().UTC().Add(time.Hour)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	scoped, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "subtree-parent-test-token", Actor: "member", AdminActor: "admin", Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: issue.UID}, ExpiresAt: &expires})
	require.NoError(t, err)
	wrong := p
	wrong.ParentTokenID = scoped.ID
	wrong.Token = "scoped-relay-test-token"
	_, err = store.CreateRelayEnrollment(ctx, wrong)
	require.Error(t, err)
	fence := store.FederationEnrollmentTransactionFence(grant, project.ID, "push")
	_, err = store.SetTeamMembership(ctx, team.UID, "member", false, "admin")
	require.NoError(t, err)
	_, err = store.AuthorizeFederationToken(ctx, created.Token, project.ID, "pull")
	require.ErrorIs(t, err, db.ErrNotFound)
	_, _, err = store.CreateIssue(db.WithAdditionalTransactionFence(ctx, fence), db.CreateIssueParams{ProjectID: project.ID, Title: "Revoked membership", Author: "member"})
	require.Error(t, err)
	_, err = store.CreateRelayEnrollment(ctx, p)
	require.Error(t, err, "exact retry still requires current authority")
	_, err = store.SetTeamMembership(ctx, team.UID, "member", true, "admin")
	require.NoError(t, err)
	grant, err = store.AuthorizeFederationToken(ctx, created.Token, project.ID, "pull")
	require.NoError(t, err)
	require.Equal(t, created.Enrollment.RelayBindingUID, grant.RelayBindingUID)
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = store.AuthorizeFederationToken(ctx, created.Token, project.ID, "pull")
	require.ErrorIs(t, err, db.ErrNotFound)
	_, _, err = store.CreateIssue(db.WithAdditionalTransactionFence(ctx, fence), db.CreateIssueParams{ProjectID: project.ID, Title: "Revoked parent", Author: "member"})
	require.Error(t, err)
	issues, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, issues, 1, "rejected grants commit no domain write")
}

// RunRelayLegacyGrantIsolation exercises relay legacy grant isolation on the supplied native store.
// A negotiated narrow grant must never be rediscovered as a legacy enrollment.
func RunRelayLegacyGrantIsolation(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: "legacy-parent-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	relay, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "legacy-relay-test-token"})
	require.NoError(t, err)
	_, err = store.FindActiveFederationEnrollment(ctx, db.ActiveFederationEnrollmentParams{ProjectID: project.ID, SpokeInstanceUID: relay.Enrollment.SpokeInstanceUID, Capabilities: relay.Enrollment.Capabilities, Actor: relay.Enrollment.Actor})
	require.ErrorIs(t, err, db.ErrNotFound, "legacy correlation must not select a negotiated bridge")
	_, err = store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{Token: relay.Token, SpokeInstanceUID: relay.Enrollment.SpokeInstanceUID, ProjectID: &project.ID, Capabilities: relay.Enrollment.Capabilities, Actor: relay.Enrollment.Actor})
	require.ErrorIs(t, err, db.ErrFederationEnrollmentTokenConflict)
	retained, err := store.AuthorizeFederationToken(ctx, relay.Token, project.ID, "pull")
	require.NoError(t, err)
	require.Equal(t, relay.Enrollment, retained)
}
