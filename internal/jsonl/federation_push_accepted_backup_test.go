package jsonl_test

import (
	"bytes"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestBackupPreservesFederationPushAccepted(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			open := func() db.Storage {
				if backend == "sqlite" {
					store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
					require.NoError(t, err)
					t.Cleanup(func() { _ = store.Close() })
					return store
				}
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				store, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { _ = store.Close() })
				return store
			}
			source := open()
			project, err := source.CreateProject(ctx, "spoke-project")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, ReplayHorizonEventID: 1, Enabled: true, PushEnabled: true, Actor: "tester"})
			require.NoError(t, err)
			_, pending, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "pending", Author: "tester"})
			require.NoError(t, err)
			_, accepted, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "accepted", Author: "tester"})
			require.NoError(t, err)
			other, err := source.CreateProject(ctx, "other-project")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: other.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 2, HubProjectUID: other.UID, ReplayHorizonEventID: 1, Enabled: true})
			require.NoError(t, err)
			require.NoError(t, source.RecordFederationPushAccepted(ctx, project.ID, []int64{accepted.ID}))
			require.NoError(t, source.RecordFederationPushAccepted(ctx, other.ID, []int64{accepted.ID}))
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
			data := bytes.Clone(backup.Bytes())
			target := open()
			require.NoError(t, jsonl.Import(ctx, &backup, target))
			found, err := target.FederationPushAccepted(ctx, project.ID, accepted.ID)
			require.NoError(t, err)
			require.True(t, found)
			found, err = target.FederationPushAccepted(ctx, other.ID, accepted.ID)
			require.NoError(t, err)
			require.True(t, found)
			found, err = target.FederationPushAccepted(ctx, other.ID, pending.ID)
			require.NoError(t, err)
			require.False(t, found)
			found, err = target.FederationPushAccepted(ctx, project.ID, pending.ID)
			require.NoError(t, err)
			require.False(t, found)
			var scoped bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &scoped, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: project.ID}))
			scopedTarget := open()
			require.NoError(t, jsonl.Import(ctx, &scoped, scopedTarget))
			found, err = scopedTarget.FederationPushAccepted(ctx, project.ID, accepted.ID)
			require.NoError(t, err)
			require.True(t, found)
			// Backups made before accepted IDs existed leave those events pending.
			legacyData := regexp.MustCompile(`,"accepted_project_ids":\[[0-9,]+\]`).ReplaceAll(data, nil)
			legacy := open()
			require.NoError(t, jsonl.Import(ctx, bytes.NewReader(legacyData), legacy))
			found, err = legacy.FederationPushAccepted(ctx, project.ID, accepted.ID)
			require.NoError(t, err)
			require.False(t, found)
			legacyBinding, err := legacy.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			legacyPending, err := legacy.PendingFederationPushEvents(ctx, project.ID, legacy.InstanceUID(), legacyBinding.PushCursorEventID, 10)
			require.NoError(t, err)
			require.Len(t, legacyPending, 2)
			clone := open()
			require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(data), clone, jsonl.ImportOptions{NewInstance: true}))
			found, err = clone.FederationPushAccepted(ctx, project.ID, accepted.ID)
			require.NoError(t, err)
			require.False(t, found)
		})
	}
}
