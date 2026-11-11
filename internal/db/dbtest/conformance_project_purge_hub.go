package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// An archived hub project can be purged when the caller asks to retire its hub
// binding. The purge removes the binding and the project's enrollments, writes
// the usual purge log, and frees the issue UIDs for another project.
func checkProjectPurgeRetiresHubBinding(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "held task", Author: "operator"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "operator", Body: "note"})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	scoped, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", ProjectID: &project.ID, Actor: "operator", Capabilities: "pull,push",
	})
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, other.ID, "operator")
	require.NoError(t, err)
	kept, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EB", ProjectID: &other.ID, Actor: "operator", Capabilities: "pull",
	})
	require.NoError(t, err)

	reason := "replaced by a new project"
	retire := db.PurgeProjectParams{ProjectID: project.ID, Actor: "operator", Reason: &reason, RetireHubBinding: true}
	_, err = store.PurgeProject(ctx, retire)
	require.ErrorIs(t, err, db.ErrProjectNotArchived, "an active project is never purged")

	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: project.ID, Actor: "operator"})
	var federated *db.ProjectFederatedError
	require.ErrorAs(t, err, &federated, "the ordinary purge still refuses a federated project")
	require.Equal(t, db.FederationRoleHub, federated.Role)

	log, err := store.PurgeProject(ctx, retire)
	require.NoError(t, err)
	require.Equal(t, project.Name, log.ProjectName)
	require.NotNil(t, log.ProjectUID)
	require.Equal(t, project.UID, *log.ProjectUID)
	require.EqualValues(t, 1, log.IssueCount)
	require.EqualValues(t, 1, log.CommentCount)
	require.Equal(t, "operator", log.Actor)
	require.NotNil(t, log.Reason)
	require.Equal(t, reason, *log.Reason)

	_, err = store.ProjectByUID(ctx, project.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.FederationBindingByProject(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	enrollments, err := store.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	var ids []int64
	for _, enrollment := range enrollments {
		ids = append(ids, enrollment.ID)
	}
	require.NotContains(t, ids, scoped.Enrollment.ID, "the purged project's enrollments are removed")
	require.Contains(t, ids, kept.Enrollment.ID, "another project's enrollment is untouched")

	reused, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, UID: issue.UID, Title: "held task", Author: "operator"})
	require.NoError(t, err, "the purged issue UID is free for another project")
	require.Equal(t, issue.UID, reused.UID)
	return nil
}

// Retiring a hub binding never covers a spoke replica: that project must
// leave its hub first.
func checkProjectPurgeKeepsSpokeRefusal(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 7,
		HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Actor: "operator",
	})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: project.ID, Actor: "operator", RetireHubBinding: true})
	var federated *db.ProjectFederatedError
	require.ErrorAs(t, err, &federated)
	require.Equal(t, db.FederationRoleSpoke, federated.Role)
	return nil
}
