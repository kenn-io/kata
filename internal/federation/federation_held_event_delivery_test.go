package federation

import (
	"path/filepath"
	"testing"
	"time"

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

func TestSyncFederationHeldEditsAfterDestinationReset(t *testing.T) {
	t.Run("links", func(t *testing.T) {
		for _, backend := range []string{"sqlite", "postgres"} {
			t.Run(backend, func(t *testing.T) {
				ctx := t.Context()
				hub, projects := newMoveDeliveryHub(t, "link-source", "link-destination", "link-absent")
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
				for _, title := range []string{"subject", "first peer", "second peer"} {
					issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: title, Author: "worker"})
					require.NoError(t, err)
					issues = append(issues, issue)
				}
				for i := range localProjects {
					require.NoError(t, sync(i))
				}
				var localIssues []db.Issue
				for _, issue := range issues {
					local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					localIssues = append(localIssues, local)
				}
				edit, err := spoke.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: localIssues[0].ID, Actor: "worker", AddBlocks: []int64{localIssues[1].ID, localIssues[2].ID}})
				require.NoError(t, err)
				require.Len(t, edit.Events, 1)
				held := edit.Events[0]
				var away []db.MoveIssueProjectOut
				for _, issue := range issues[1:] {
					move, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
					require.NoError(t, err)
					away = append(away, move)
				}
				require.ErrorContains(t, sync(0), "federation_project_not_enrolled")
				_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issues[1].ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: away[0].Issue.Revision, Actor: "worker"})
				require.NoError(t, err)
				require.NoError(t, sync(1))
				require.ErrorContains(t, sync(0), "federation_project_not_enrolled")
				binding, err := spoke.FederationBindingByProject(ctx, localProjects[1].ID)
				require.NoError(t, err)
				require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, localProjects[1].ID, 0, 0, spoke.InstanceUID(), binding.PushCursorEventID))
				require.NoError(t, sync(1))
				_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issues[2].ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: away[1].Issue.Revision, Actor: "worker"})
				require.NoError(t, err)
				require.NoError(t, sync(1))
				require.NoError(t, sync(0))
				delivered, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{held.UID})
				require.NoError(t, err)
				require.Len(t, delivered, 1)
				require.Equal(t, held.ContentHash, delivered[0].ContentHash)
			})
		}
	})
}

func TestSyncAfterApprovedMoveReturnsBeforeLeave(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "leave-source", "leave-target", "leave-away")
			spoke, local, credentials := newHeldEventSpoke(t, backend, hub, projects)
			t.Cleanup(func() { _ = spoke.Close() })
			sync := func(index int) error {
				binding, err := spoke.FederationBindingByProject(ctx, local[index].ID)
				if err != nil {
					return err
				}
				return SyncFederationOnce(ctx, spoke, binding, credentials[local[index].UID], credentials)
			}
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moved", Author: "worker"})
			require.NoError(t, err)
			for i := range local {
				require.NoError(t, sync(i))
			}
			localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			localMove, err := spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: local[0].ID, ToProjectID: local[1].ID, IfMatchRev: localIssue.Revision, Actor: "worker"})
			require.NoError(t, err)
			// Give the hub move a later physical clock than the offline move.
			if delay := time.Until(time.UnixMilli(localMove.Event.HLCPhysicalMS + 1)); delay > 0 {
				//nolint:kennlint // sleeptest: Both stores use real clocks to order the hub move after the offline move.
				time.Sleep(delay)
			}
			away, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			remote := db.RemoteEventFromStored(*away.Event)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{remote})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			require.NoError(t, spoke.(db.RelayDisconnectFenceStore).FenceFederationLeave(ctx, local[0].ID))
			returned, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: away.Issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			remote = db.RemoteEventFromStored(*returned.Event)
			_, err = spoke.InsertRemoteEvents(ctx, local[1].ID, []db.RemoteEvent{remote})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[1].ID))
			_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
			require.NoError(t, err)
			delete(credentials, local[0].UID)
			require.NoError(t, sync(1))
		})
	}
}
