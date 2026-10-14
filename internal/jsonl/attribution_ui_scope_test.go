package jsonl

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

// R5/R6: local browser cursors belong to owner backup, never a portable project.
func TestAttributionUIResetExportScope(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store transitionScopeStore
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
			var keys []string
			for _, project := range []db.Project{selected, private} {
				key := "attribution_ui_reset." + project.UID
				keys = append(keys, key)
				q := `INSERT INTO meta(key,value) VALUES(?,?)`
				if backend == "postgres" {
					q = `INSERT INTO meta(key,value) VALUES($1,$2)`
				}
				_, err = store.ExecContext(t.Context(), q, key, "777")
				require.NoError(t, err)
			}
			for _, cutover := range []bool{false, true} {
				if cutover && backend == "postgres" {
					continue
				}
				name := "native"
				if cutover {
					name = "cutover"
				}
				t.Run(name, func(t *testing.T) {
					var output bytes.Buffer
					opts := ExportOptions{ProjectID: selected.ID}
					if cutover {
						err = exportForCutover(t.Context(), store, &output, opts)
					} else {
						err = Export(t.Context(), store, &output, opts)
					}
					require.NoError(t, err)
					for _, key := range keys {
						require.NotContains(t, output.String(), key, "project exports exclude even their own local UI reset state")
					}
				})
			}
			var owner bytes.Buffer
			require.NoError(t, Export(t.Context(), store, &owner, ExportOptions{}))
			for _, key := range keys {
				require.Contains(t, owner.String(), key, "owner backups retain durable local cursors")
			}
		})
	}
}
