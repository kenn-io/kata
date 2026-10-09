package jsonl

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

type relayResetScopeStore interface {
	db.Storage
	exportQuerier
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// R6/A2: project exports omit hub-local reset translations, including the
// selected project, while complete owner backups retain them.
func TestRelayResetMetadataScope(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store relayResetScopeStore
			if backend == "sqlite" {
				native, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "scope.db"))
				require.NoError(t, err)
				store = native
			} else {
				dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
				t.Cleanup(cleanup)
				native, err := pgstore.Open(t.Context(), dsn)
				require.NoError(t, err)
				store = native
			}
			t.Cleanup(func() { _ = store.Close() })
			selected, err := store.CreateProject(t.Context(), "selected-project")
			require.NoError(t, err)
			private, err := store.CreateProject(t.Context(), "private-project")
			require.NoError(t, err)
			key := "relay_reset." + private.UID
			query := `INSERT INTO meta(key,value) VALUES(?,?)`
			if backend == "postgres" {
				query = `INSERT INTO meta(key,value) VALUES($1,$2)`
			}
			_, err = store.ExecContext(t.Context(), query, "relay_reset."+selected.UID, "selected-local-hop-canary")
			require.NoError(t, err)
			_, err = store.ExecContext(t.Context(), query, key, "private-reset-canary")
			require.NoError(t, err)
			_, err = store.ExecContext(t.Context(), query, "federation.pending_creation."+selected.UID+".issue.local-uid", "selected-pending-intent-canary")
			require.NoError(t, err)
			_, err = store.ExecContext(t.Context(), query, "federation.pending_creation."+private.UID+".comment.private-uid", "private-pending-intent-canary")
			require.NoError(t, err)
			for _, cutover := range []bool{false, true} {
				if cutover && backend == "postgres" {
					continue
				} // The legacy exporter reads SQLite only.
				name := "current_storage"
				if cutover {
					name = "forward_cutover"
				}
				t.Run(name, func(t *testing.T) {
					var output bytes.Buffer
					options := ExportOptions{ProjectID: selected.ID}
					if cutover {
						err = exportForCutover(t.Context(), store, &output, options)
					} else {
						err = Export(t.Context(), store, &output, options)
					}
					require.NoError(t, err)
					require.NotContains(t, output.String(), "private-reset-canary")
					require.NotContains(t, output.String(), key)
					require.NotContains(t, output.String(), "selected-local-hop-canary")
					require.NotContains(t, output.String(), "selected-pending-intent-canary")
					require.NotContains(t, output.String(), "private-pending-intent-canary")
				})
			}
			var owner bytes.Buffer
			require.NoError(t, Export(t.Context(), store, &owner, ExportOptions{}))
			require.Contains(t, owner.String(), "private-reset-canary")
			require.Contains(t, owner.String(), "selected-pending-intent-canary")
			require.Contains(t, owner.String(), "private-pending-intent-canary")
		})
	}
}
