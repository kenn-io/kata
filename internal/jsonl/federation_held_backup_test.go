package jsonl_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestHeldEditSurvivesBackupAndResets(t *testing.T) {
	for _, sourceBackend := range []string{"sqlite", "postgres"} {
		t.Run("source-"+sourceBackend, func(t *testing.T) {
			ctx := t.Context()
			hub := openExportTestDB(t)
			var spoke db.Storage
			if sourceBackend == "postgres" {
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				var err error
				spoke, err = pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { _ = spoke.Close() })
			} else {
				spoke = openImportTargetDB(t)
			}
			var remote, local []db.Project
			for i, name := range []string{"backup-source", "backup-destination", "backup-absent"} {
				project, err := hub.CreateProject(ctx, name)
				require.NoError(t, err)
				_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
				require.NoError(t, err)
				remote = append(remote, project)
				if i == 2 {
					continue
				}
				replica, err := spoke.CreateProjectWithUID(ctx, name, project.UID)
				require.NoError(t, err)
				_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
				local = append(local, replica)
			}

			issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: remote[0].ID, Title: "backup held edit", Author: "worker"})
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{db.RemoteEventFromStored(created)})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, acknowledged, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localIssue.ID, Author: "worker", Body: "acknowledged"})
			require.NoError(t, err)
			require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, local[0].ID, acknowledged.ID))
			_, held, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localIssue.ID, Author: "worker", Body: "held"})
			require.NoError(t, err)
			move, err := spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: local[0].ID, ToProjectID: local[1].ID, IfMatchRev: localIssue.Revision, Actor: "worker"})
			require.NoError(t, err)
			away, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[0].ID, ToProjectID: remote[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			awayEvent := db.RemoteEventFromStored(*away.Event)
			awayEvent.HLCPhysicalMS, awayEvent.HLCCounter = move.Event.HLCPhysicalMS, move.Event.HLCCounter+1
			awayEvent.ContentHash, err = db.RemoteEventContentHash(awayEvent)
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{awayEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
			require.NoError(t, err)
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, spoke, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
			for _, backend := range []string{"sqlite", "postgres"} {
				t.Run(backend, func(t *testing.T) {
					var target db.Storage
					var err error
					if backend == "postgres" {
						dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
						t.Cleanup(cleanup)
						target, err = pgstore.Open(ctx, dsn)
					} else {
						target, err = sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
					}
					require.NoError(t, err)
					t.Cleanup(func() { _ = target.Close() })
					require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(backup.Bytes()), target, jsonl.ImportOptions{}))
					origin := target.InstanceUID()
					require.NoError(t, target.ResetFederatedProjectIfNoPendingPush(ctx, local[1].ID, 0, 0, origin, 0))
					parked, err := target.ProjectByUID(ctx, remote[2].UID)
					require.NoError(t, err)
					_, _, _, err = target.RestoreProject(ctx, parked.ID, "worker")
					require.NoError(t, err)
					_, err = target.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: parked.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: remote[2].ID, HubProjectUID: remote[2].UID, Actor: "worker", Enabled: true, PushEnabled: true})
					require.NoError(t, err)
					require.NoError(t, target.ResetFederatedProjectIfNoPendingPush(ctx, parked.ID, 0, 0, origin, 0))
					_, err = target.InsertRemoteEvents(ctx, parked.ID, []db.RemoteEvent{db.RemoteEventFromStored(created), awayEvent})
					require.NoError(t, err)
					require.NoError(t, target.MaterializeFederatedProject(ctx, parked.ID))
					require.NoError(t, target.ResetFederatedProjectIfNoPendingPush(ctx, parked.ID, 0, 0, origin, 0))
					_, err = target.EventsByUIDs(ctx, local[0].ID, []string{acknowledged.UID})
					require.ErrorIs(t, err, db.ErrNotFound)
					retained, err := target.EventsByUIDs(ctx, local[0].ID, []string{held.UID})
					require.NoError(t, err)
					require.Len(t, retained, 1)
					require.Equal(t, held.UID, retained[0].UID)
					require.Equal(t, held.ContentHash, retained[0].ContentHash)
				})
			}
		})
	}
}
