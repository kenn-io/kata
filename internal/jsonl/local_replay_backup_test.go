package jsonl_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestBackupPreservesLocalReplayDebt(t *testing.T) {
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
			var events []db.Event
			for _, title := range []string{"pending", "unprojected", "ordinary"} {
				_, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "tester"})
				require.NoError(t, err)
				events = append(events, event)
			}
			sqlStore := source.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			for _, event := range events[:2] {
				_, err := sqlStore.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, event.UID)
				require.NoError(t, err)
				inserted, err := source.InsertRemoteEvent(ctx, project.ID, db.RemoteEvent{EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt, Payload: jsontext.Value(event.Payload), ContentHash: event.ContentHash, IssueUID: event.IssueUID})
				require.NoError(t, err)
				require.True(t, inserted)
			}
			due, err := source.FederationLocalReplayDue(ctx, events[0].UID, true)
			require.NoError(t, err)
			require.True(t, due)
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
			data := bytes.Clone(backup.Bytes())
			target := open()
			require.NoError(t, jsonl.Import(ctx, &backup, target))
			due, err = target.FederationLocalReplayDue(ctx, events[0].UID, false)
			require.NoError(t, err)
			require.True(t, due, "an acknowledged replay retains its pending notification")
			due, err = target.FederationLocalReplayDue(ctx, events[1].UID, true)
			require.NoError(t, err)
			require.True(t, due, "an unprojected replay remains eligible for recovery")
			due, err = target.FederationLocalReplayDue(ctx, events[2].UID, true)
			require.NoError(t, err)
			require.False(t, due, "ordinary echoes stay silent")

			clone := open()
			require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(data), clone, jsonl.ImportOptions{NewInstance: true}))
			due, err = clone.FederationLocalReplayDue(ctx, events[0].UID, true)
			require.NoError(t, err)
			require.False(t, due, "a new instance does not inherit another instance's replay debt")
		})
	}
}
