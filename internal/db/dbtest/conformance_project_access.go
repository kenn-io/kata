package dbtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunProjectAccessConformance exercises the authenticated default from R2:
// an existing project is visible to an authenticated actor, never anonymous.
func RunProjectAccessConformance(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	access, ok := store.(interface {
		AccessibleProjectUIDs(context.Context, string) ([]string, error)
	})
	require.True(t, ok, "backend must implement project access decisions")
	ids, err := access.AccessibleProjectUIDs(ctx, "member")
	require.NoError(t, err)
	require.Equal(t, []string{project.UID}, ids)
	ids, err = access.AccessibleProjectUIDs(ctx, "")
	require.NoError(t, err)
	require.Empty(t, ids, "all visibility means authenticated users")
	ids, err = store.AnonymousAccessibleProjectUIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{project.UID}, ids, "anonymous reads see all-visibility projects")
	ids, err = store.AnonymousAccessibleProjectUIDs(db.WithAuthorizedProjects(ctx, []string{}))
	require.NoError(t, err)
	require.Empty(t, ids, "anonymous reads retain an existing empty project boundary")
	ids, err = store.AnonymousAccessibleProjectUIDs(db.WithAuthorizedProjects(ctx, []string{project.UID}))
	require.NoError(t, err)
	require.Equal(t, []string{project.UID}, ids, "anonymous reads intersect an existing project boundary")

	policy, ok := store.(db.ProjectAccessStorage)
	require.True(t, ok, "backend must provide audited team and policy storage")
	team, event, err := policy.CreateTeam(ctx, "engineering", "admin")
	require.NoError(t, err)
	require.Len(t, team.UID, 26)
	require.Equal(t, int64(1), team.Revision)
	assertProjectAccessAudit(t, store, event, "team.created")
	gotTeam, err := policy.TeamByUID(ctx, team.UID)
	require.NoError(t, err)
	require.Equal(t, team, gotTeam)
	teams, err := policy.ListTeams(ctx)
	require.NoError(t, err)
	require.Equal(t, []db.Team{team}, teams)
	initial, err := policy.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, "all", initial.Visibility)
	initial.Visibility = "teams"
	initial.TeamUIDs = []string{team.UID}
	restricted, event, err := policy.SetProjectAccessPolicy(ctx, initial, "admin")
	require.NoError(t, err)
	require.Greater(t, restricted.Revision, initial.Revision)
	assertProjectAccessAudit(t, store, event, "project.access_changed")
	assertProjectAccessUIDs(t, access, "member", nil)
	assertProjectAccessUIDs(t, access, "outsider", nil)
	ids, err = store.AnonymousAccessibleProjectUIDs(ctx)
	require.NoError(t, err)
	require.Empty(t, ids, "anonymous reads do not inherit team memberships")
	event, err = policy.SetTeamMembership(ctx, team.UID, "member", true, "admin")
	require.NoError(t, err)
	assertProjectAccessAudit(t, store, event, "team.membership_changed")
	assertProjectAccessUIDs(t, access, "member", []string{project.UID})
	members, err := policy.TeamMembers(ctx, team.UID)
	require.NoError(t, err)
	require.Equal(t, []string{"member"}, members)
	revision, err := policy.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	event, err = policy.SetTeamMembership(ctx, team.UID, "member", true, "admin")
	require.NoError(t, err)
	require.Empty(t, event.UID, "idempotent membership has no audit mutation")
	unchangedRevision, err := policy.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, revision, unchangedRevision)

	// Membership in any selected team grants access; removing one never admits outsiders.
	secondary, _, err := policy.CreateTeam(ctx, "secondary-team", "admin")
	require.NoError(t, err)
	_, err = policy.SetTeamMembership(ctx, secondary.UID, "second-member", true, "admin")
	require.NoError(t, err)
	restricted.TeamUIDs = append(restricted.TeamUIDs, secondary.UID)
	restricted, _, err = policy.SetProjectAccessPolicy(ctx, restricted, "admin")
	require.NoError(t, err)
	assertProjectAccessUIDs(t, access, "second-member", []string{project.UID})
	assertProjectAccessUIDs(t, access, "member", []string{project.UID})
	assertProjectAccessUIDs(t, access, "outsider", nil)
	_, err = policy.DeleteTeam(ctx, secondary.UID, "admin")
	require.NoError(t, err)
	assertProjectAccessUIDs(t, access, "second-member", nil)
	assertProjectAccessUIDs(t, access, "member", []string{project.UID})
	restricted, err = policy.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)

	var exported []db.ImportRecord
	for record, err := range policy.ExportProjectAccess(ctx) {
		require.NoError(t, err)
		exported = append(exported, record)
	}
	require.Len(t, exported, 3)
	require.IsType(t, &db.Team{}, exported[0])
	require.IsType(t, &db.TeamMembership{}, exported[1])
	require.Equal(t, &restricted, exported[2])

	// Two tokens and rotation remain the same canonical member.
	for _, secret := range []string{"test-user-token-one", "test-user-token-two"} {
		token, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
			PlaintextToken: secret, Actor: "member", AdminActor: "admin",
		})
		require.NoError(t, err)
		assertProjectAccessUIDs(t, access, token.Actor, []string{project.UID})
		_, _, err = store.RevokeAPIToken(ctx, token.ID, "admin")
		require.NoError(t, err)
	}
	assertProjectAccessUIDs(t, access, "member", []string{project.UID})

	// Validation failures cannot broaden or partly replace the current policy.
	for _, candidate := range []db.ProjectAccessPolicy{
		{ProjectUID: project.UID, Visibility: "unknown"},
		{ProjectUID: project.UID, Visibility: "all", TeamUIDs: []string{team.UID}},
		{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{"missing"}},
		{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID, team.UID}},
	} {
		_, _, err := policy.SetProjectAccessPolicy(ctx, candidate, "admin")
		require.Error(t, err)
		assertProjectAccessUIDs(t, access, "outsider", nil)
	}
	_, _, err = policy.SetProjectAccessPolicy(ctx, initial, "admin")
	require.Error(t, err, "stale policy revision cannot replace current policy")
	_, err = policy.SetTeamMembership(ctx, team.UID, "", true, "admin")
	require.Error(t, err)
	_, err = policy.SetTeamMembership(ctx, "missing", "member", true, "admin")
	require.Error(t, err)
	_, _, err = policy.CreateTeam(ctx, "engineering", "admin")
	require.Error(t, err)
	_, _, err = policy.CreateTeam(ctx, "", "admin")
	require.Error(t, err)
	_, _, err = policy.CreateTeam(ctx, "other", "")
	require.Error(t, err)

	event, err = policy.MigrateTeamActor(ctx, "member", "renamed-member", "admin")
	require.NoError(t, err)
	assertProjectAccessAudit(t, store, event, "team.actor_migrated")
	assertProjectAccessUIDs(t, access, "member", nil)
	assertProjectAccessUIDs(t, access, "renamed-member", []string{project.UID})

	// A fence admitted before removal must recheck inside the mutation.
	fenced := db.WithAdditionalTransactionFence(ctx,
		policy.ProjectAccessTransactionFence("renamed-member", []string{project.UID}))
	event, err = policy.SetTeamMembership(ctx, team.UID, "renamed-member", false, "admin")
	require.NoError(t, err)
	assertProjectAccessAudit(t, store, event, "team.membership_changed")
	_, _, err = store.CreateIssue(fenced, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Must not commit", Author: "renamed-member",
	})
	require.ErrorIs(t, err, db.ErrNotFound)
	assertProjectAccessUIDs(t, access, "renamed-member", nil)

	_, err = policy.SetTeamMembership(ctx, team.UID, "member", true, "admin")
	require.NoError(t, err)
	event, err = policy.DeleteTeam(ctx, team.UID, "admin")
	require.NoError(t, err)
	assertProjectAccessAudit(t, store, event, "team.deleted")
	assertProjectAccessUIDs(t, access, "member", nil)
	empty, err := policy.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, "teams", empty.Visibility, "deleted last team never widens access")
	require.Empty(t, empty.TeamUIDs)
	_, err = policy.TeamByUID(ctx, team.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	empty.Visibility = "all"
	_, _, err = policy.SetProjectAccessPolicy(ctx, empty, "admin")
	require.NoError(t, err)
	assertProjectAccessUIDs(t, access, "outsider", []string{project.UID})
	assertProjectAccessUIDs(t, access, "", nil)

	// A merge can remove an admitted source while leaving its target outside the
	// member's policy. The revision invalidates streams that filter project.merged.
	mergePolicy, ok := store.(db.ProjectAccessStorage)
	require.True(t, ok)
	mergeSource, err := store.CreateProject(ctx, "merge-visible-source")
	require.NoError(t, err)
	mergeTarget, err := store.CreateProject(ctx, "merge-hidden-target")
	require.NoError(t, err)
	sourceTeam, _, err := mergePolicy.CreateTeam(ctx, "merge-source-team", "admin")
	require.NoError(t, err)
	targetTeam, _, err := mergePolicy.CreateTeam(ctx, "merge-target-team", "admin")
	require.NoError(t, err)
	_, err = mergePolicy.SetTeamMembership(ctx, sourceTeam.UID, "merge-member", true, "admin")
	require.NoError(t, err)
	_, err = mergePolicy.SetTeamMembership(ctx, targetTeam.UID, "merge-outsider", true, "admin")
	require.NoError(t, err)
	sourcePolicy, err := mergePolicy.ProjectAccessPolicy(ctx, mergeSource.UID)
	require.NoError(t, err)
	sourcePolicy.Visibility, sourcePolicy.TeamUIDs = "teams", []string{sourceTeam.UID}
	_, _, err = mergePolicy.SetProjectAccessPolicy(ctx, sourcePolicy, "admin")
	require.NoError(t, err)
	targetPolicy, err := mergePolicy.ProjectAccessPolicy(ctx, mergeTarget.UID)
	require.NoError(t, err)
	targetPolicy.Visibility, targetPolicy.TeamUIDs = "teams", []string{targetTeam.UID}
	_, _, err = mergePolicy.SetProjectAccessPolicy(ctx, targetPolicy, "admin")
	require.NoError(t, err)
	mergeVisibleUIDs, err := access.AccessibleProjectUIDs(ctx, "merge-member")
	require.NoError(t, err)
	require.Contains(t, mergeVisibleUIDs, mergeSource.UID)
	require.NotContains(t, mergeVisibleUIDs, mergeTarget.UID)
	beforeMergeRevision, err := mergePolicy.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: mergeSource.ID, TargetProjectID: mergeTarget.ID, Actor: "admin"})
	require.NoError(t, err)
	afterMergeRevision, err := mergePolicy.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Greater(t, afterMergeRevision, beforeMergeRevision, "merge advances the revision used by project access stream fences")
	mergeVisibleUIDs, err = access.AccessibleProjectUIDs(ctx, "merge-member")
	require.NoError(t, err)
	require.NotContains(t, mergeVisibleUIDs, mergeSource.UID)
	require.NotContains(t, mergeVisibleUIDs, mergeTarget.UID)
}

// RunProjectAccessTokenEnrollment exercises project access token enrollment on the supplied native store.
// Token enrollment and memberships must commit together (R2, Task 1).
func RunProjectAccessTokenEnrollment(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	team, _, err := store.CreateTeam(ctx, "token-team", "admin")
	require.NoError(t, err)
	params := db.CreateAPITokenParams{PlaintextToken: "test-enrolled-user-token", Actor: "  new-member  ", AdminActor: "admin"}
	params.TeamUIDs = []string{team.UID, "00000000000000000000000000"}
	_, _, err = store.CreateAPIToken(ctx, params)
	require.Error(t, err)
	_, err = store.ResolveAPIToken(ctx, params.PlaintextToken)
	require.ErrorIs(t, err, db.ErrNotFound)
	members, err := store.TeamMembers(ctx, team.UID)
	require.NoError(t, err)
	require.Empty(t, members, "partial enrollment must roll back")
	params.TeamUIDs = []string{team.UID}
	token, event, err := store.CreateAPIToken(ctx, params)
	require.NoError(t, err)
	assertProjectAccessAudit(t, store, event, "token.created")
	members, err = store.TeamMembers(ctx, team.UID)
	require.NoError(t, err)
	require.Equal(t, []string{"new-member"}, members)
	_, _, err = store.RevokeAPIToken(ctx, token.ID, "admin")
	require.NoError(t, err)
	members, err = store.TeamMembers(ctx, team.UID)
	require.NoError(t, err)
	require.Equal(t, []string{"new-member"}, members)
	for _, uids := range [][]string{{team.UID, team.UID}, {"invalid"}} {
		params.PlaintextToken = "test-invalid-enrollment"
		params.TeamUIDs = uids
		_, _, err = store.CreateAPIToken(ctx, params)
		require.Error(t, err)
	}
}

func assertProjectAccessUIDs(t *testing.T, access interface {
	AccessibleProjectUIDs(context.Context, string) ([]string, error)
}, actor string, expected []string) {
	t.Helper()
	ids, err := access.AccessibleProjectUIDs(context.Background(), actor)
	require.NoError(t, err)
	assert.ElementsMatch(t, expected, ids)
}

func assertProjectAccessAudit(t *testing.T, store db.Storage, event db.Event, kind string) {
	t.Helper()
	system, err := store.SystemProject(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, event.UID)
	require.Equal(t, system.ID, event.ProjectID, "policy audit stays daemon-local")
	require.Equal(t, kind, event.Type)
	require.Equal(t, "admin", event.Actor)
}

// RunProjectAccessMergeIsolation exercises project access merge isolation on the supplied native store.
func RunProjectAccessMergeIsolation(t *testing.T, store db.Storage) {
	t.Helper()
	// A peer snapshot may not manufacture hub-local administrative authority.
	for _, record := range []db.ImportRecord{
		&db.Team{UID: "00000000000000000000000001", Name: "peer-team", Revision: 1},
		&db.TeamMembership{TeamUID: "00000000000000000000000001", Actor: "member"},
		&db.ProjectAccessPolicy{ProjectUID: "00000000000000000000000002", Visibility: "all", Revision: 1},
	} {
		err := store.ImportReplay(t.Context(), []db.ImportRecord{&db.ProjectExport{ID: 42, UID: "00000000000000000000000002", Name: "peer-project"}, record}, db.ImportOptions{MergeProject: true})
		require.ErrorContains(t, err, "hub-local", "valid project transfer must refuse hub-local authority")
		teams, err := store.ListTeams(t.Context())
		require.NoError(t, err)
		require.Empty(t, teams)
	}
}
