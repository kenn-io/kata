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
	"go.kenn.io/kata/internal/testenv"
)

func newHeldEventSpoke(t *testing.T, backend string, hub *testenv.Env, projects []db.Project) (db.Storage, []db.Project, moveCredentialStore) {
	t.Helper()
	ctx := t.Context()
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
	return spoke, localProjects, credentials
}

func TestSyncFederationReturnedEditWhileAnotherHeld(t *testing.T) {
	t.Run("linked issues", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "reset-source", "reset-destination", "reset-absent")
		spoke := newMoveDeliverySpoke(t, hub, projects[:2])
		first, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "first", Author: "worker"})
		require.NoError(t, err)
		second, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "second", Author: "worker"})
		require.NoError(t, err)
		for i := range spoke.projects {
			require.NoError(t, spoke.sync(t, i))
		}
		localFirst, err := spoke.env.DB.IssueByUID(ctx, first.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localSecond, err := spoke.env.DB.IssueByUID(ctx, second.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, held, err := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: localFirst.ID, ToIssueID: localSecond.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: localFirst.ID, FromShortID: localFirst.ShortID, FromUID: localFirst.UID, ToShortID: localSecond.ShortID, ToUID: localSecond.UID, Actor: "worker"})
		require.NoError(t, err)
		firstAway, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: first.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: first.Revision, Actor: "worker"})
		require.NoError(t, err)
		secondAway, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: second.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: second.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.ErrorContains(t, spoke.sync(t, 0), "federation_project_not_enrolled")
		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: first.ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: firstAway.Issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 1))
		require.ErrorContains(t, spoke.sync(t, 0), "federation_project_not_enrolled")
		_, err = hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{held.UID})
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: second.ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: secondAway.Issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 1))
		require.NoError(t, spoke.sync(t, 0))
		events, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{held.UID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, held.ContentHash, events[0].ContentHash)
	})
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "held-source", "held-destination", "held-absent")
			spoke, localProjects, credentials := newHeldEventSpoke(t, backend, hub, projects)
			t.Cleanup(func() { _ = spoke.Close() })
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
			_, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issues[1].ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: away[1].Issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, sync(1))
			require.ErrorContains(t, sync(0), "federation_project_not_enrolled")
			retained, err := spoke.EventsByUIDs(ctx, localProjects[0].ID, []string{comments[0].UID})
			require.NoError(t, err)
			require.Equal(t, comments[0].ContentHash, retained[0].ContentHash)
			delivered, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{comments[1].UID})
			require.NoError(t, err)
			require.Equal(t, comments[1].ContentHash, delivered[0].ContentHash)
			require.ErrorContains(t, sync(0), "federation_project_not_enrolled")
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
