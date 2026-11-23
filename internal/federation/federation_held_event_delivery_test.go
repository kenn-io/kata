package federation

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestSyncFederationReturnedEditWhileAnotherHeld(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "held-source", "held-destination", "held-absent")
			var open func() (db.Storage, error)
			if backend == "postgres" {
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				open = func() (db.Storage, error) { return pgstore.Open(ctx, dsn) }
			} else {
				path := filepath.Join(t.TempDir(), "spoke.db")
				open = func() (db.Storage, error) { return sqlitestore.Open(ctx, path) }
			}
			spoke, err := open()
			require.NoError(t, err)
			t.Cleanup(func() { _ = spoke.Close() })
			credentials := newMoveCredentialStore()
			var localProjects []db.Project
			for _, project := range projects[:2] {
				local, err := spoke.CreateProjectWithUID(ctx, project.Name, project.UID)
				require.NoError(t, err)
				localProjects = append(localProjects, local)
				_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
				require.NoError(t, err)
				grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
				credentials[project.UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token, Actor: "worker"}
			}
			sync := func(index int) error {
				binding, err := spoke.FederationBindingByProject(ctx, localProjects[index].ID)
				if err != nil {
					return err
				}
				return SyncFederationOnce(ctx, spoke, binding, credentials[localProjects[index].UID], credentials)
			}
			var issues []db.Issue
			for _, title := range []string{"first", "second"} {
				issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: title, Author: "worker"})
				require.NoError(t, err)
				issues = append(issues, issue)
			}
			for i := range localProjects {
				require.NoError(t, sync(i))
			}
			var comments []db.Event
			var away []db.MoveIssueProjectOut
			for _, issue := range issues {
				local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: issue.Title})
				require.NoError(t, err)
				comments = append(comments, comment)
				move, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
				require.NoError(t, err)
				away = append(away, move)
			}
			require.ErrorContains(t, sync(0), "federation_project_not_enrolled")
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issues[1].ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: away[1].Issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, sync(1))
			require.ErrorContains(t, sync(0), "federation_project_not_enrolled")
			retained, err := spoke.EventsByUIDs(ctx, localProjects[0].ID, []string{comments[0].UID})
			require.NoError(t, err)
			require.Equal(t, comments[0].ContentHash, retained[0].ContentHash)
			delivered, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{comments[1].UID})
			require.NoError(t, err)
			require.Equal(t, comments[1].ContentHash, delivered[0].ContentHash)
			purged, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "reset", Author: "worker"})
			require.NoError(t, err)
			_, _, _, err = hub.DB.SoftDeleteIssue(ctx, purged.ID, "worker")
			require.NoError(t, err)
			_, err = hub.DB.PurgeIssue(ctx, purged.ID, "worker", nil)
			require.NoError(t, err)
			binding, err := spoke.FederationBindingByProject(ctx, localProjects[0].ID)
			require.NoError(t, err)
			require.NoError(t, SyncFederationPullOnlyOnce(ctx, spoke, binding, credentials[localProjects[0].UID], credentials))
			status, err := spoke.FederationSyncStatusByProject(ctx, localProjects[0].ID)
			require.NoError(t, err)
			require.NotNil(t, status.LastResetAt)
			require.NoError(t, spoke.Close())
			spoke, err = open()
			require.NoError(t, err)
			require.ErrorContains(t, sync(0), "federation_project_not_enrolled")
			retained, err = spoke.EventsByUIDs(ctx, localProjects[0].ID, []string{comments[0].UID})
			require.NoError(t, err)
			require.Equal(t, comments[0].ContentHash, retained[0].ContentHash)
			hubComments, err := hub.DB.CommentsByIssue(ctx, issues[1].ID)
			require.NoError(t, err)
			require.Len(t, hubComments, 1)
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issues[0].ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: away[0].Issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, sync(1))
			require.NoError(t, sync(0))
			delivered, err = hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{comments[0].UID})
			require.NoError(t, err)
			require.Equal(t, comments[0].ContentHash, delivered[0].ContentHash)
		})
	}
}
