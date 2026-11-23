package jsonl

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationAcknowledgmentMetadataProjectExport(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var store transitionScopeStore
			if backend == "sqlite" {
				native, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
				require.NoError(t, err)
				store = native
			} else {
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				native, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				store = native
			}
			t.Cleanup(func() { _ = store.Close() })
			selected, err := store.CreateProject(ctx, "selected-project")
			require.NoError(t, err)
			other, err := store.CreateProject(ctx, "other-project")
			require.NoError(t, err)
			var keys []string
			for _, project := range []db.Project{selected, other} {
				issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "issue", Author: "worker"})
				require.NoError(t, err)
				require.NotEmpty(t, issue.UID)
				key := db.FederationAcknowledgedEventPrefix + project.UID + "." + event.UID
				query := `INSERT INTO meta(key,value) VALUES(?,?)`
				if backend == "postgres" {
					query = `INSERT INTO meta(key,value) VALUES($1,$2)`
				}
				_, err = store.ExecContext(ctx, query, key, "1")
				require.NoError(t, err)
				keys = append(keys, key)
				fence := db.FederationLeaveFencePrefix + project.UID
				_, err = store.ExecContext(ctx, query, fence, "{}")
				require.NoError(t, err)
				keys = append(keys, fence)
			}
			for _, cutover := range []bool{false, true} {
				if cutover && backend == "postgres" {
					continue
				}
				name := "storage"
				if cutover {
					name = "cutover"
				}
				t.Run(name, func(t *testing.T) {
					var scoped, full bytes.Buffer
					var err error
					if cutover {
						err = exportForCutover(ctx, store, &scoped, ExportOptions{ProjectID: selected.ID})
					} else {
						err = Export(ctx, store, &scoped, ExportOptions{ProjectID: selected.ID})
					}
					require.NoError(t, err)
					if cutover {
						err = exportForCutover(ctx, store, &full, ExportOptions{})
					} else {
						err = Export(ctx, store, &full, ExportOptions{})
					}
					require.NoError(t, err)
					for _, key := range keys {
						require.NotContains(t, scoped.String(), key)
						require.Contains(t, full.String(), key)
					}
					restored, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
					require.NoError(t, err)
					t.Cleanup(func() { _ = restored.Close() })
					require.NoError(t, Import(ctx, bytes.NewReader(full.Bytes()), restored))
					var restoredKeys []string
					for record, err := range restored.ExportMeta(ctx) {
						require.NoError(t, err)
						restoredKeys = append(restoredKeys, record.Key)
					}
					for _, key := range keys {
						require.Contains(t, restoredKeys, key)
					}
				})
			}
		})
	}
}
