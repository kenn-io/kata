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

func TestBackupPreservesFederationPushAccepted(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			open := func() db.Storage {
				if backend == "sqlite" {
					s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
					require.NoError(t, err)
					t.Cleanup(func() { _ = s.Close() })
					return s
				}
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { _ = s.Close() })
				return s
			}
			source := open()
			project, err := source.CreateProject(ctx, "spoke")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
			require.NoError(t, err)
			_, pending, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "pending", Author: "tester"})
			require.NoError(t, err)
			_, accepted, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "accepted", Author: "tester"})
			require.NoError(t, err)
			require.NoError(t, source.RecordFederationPushAccepted(ctx, project.ID, []int64{accepted.ID}))
			_, skipped, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "skipped", Author: "tester"})
			require.NoError(t, err)
			q, err := source.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{ProjectID: project.ID, Direction: db.FederationQuarantineDirectionPush, FirstEventID: skipped.ID, LastEventID: skipped.ID, EventUIDs: []string{skipped.UID}, Error: "poison"})
			require.NoError(t, err)
			_, err = source.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{ID: q.ID, ProjectID: project.ID, Actor: "tester"})
			require.NoError(t, err)
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
			data := bytes.Clone(backup.Bytes())
			target := open()
			require.NoError(t, jsonl.Import(ctx, &backup, target))
			binding, err := target.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			events, err := target.PendingFederationPushEvents(ctx, project.ID, target.InstanceUID(), binding.PushCursorEventID, 10)
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, pending.UID, events[0].UID)
			clone := open()
			require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(data), clone, jsonl.ImportOptions{NewInstance: true}))
			found, err := clone.FederationPushAccepted(ctx, project.ID, accepted.ID)
			require.NoError(t, err)
			require.False(t, found, "a new instance must not inherit acknowledgments")
		})
	}
}
