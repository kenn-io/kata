package jsonl_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// Select the legacy SQL exporter with a complete fixture both paths can read.
// The embedded store still supplies the source's real raw SQL metadata.
type spokeNamesLegacyExportStore struct{ *sqlitestore.Store }

func (spokeNamesLegacyExportStore) SchemaVersion(context.Context) (int, error) {
	return db.CurrentSchemaVersion() - 1, nil
}

func TestSpokeNamesProjectExport(t *testing.T) {
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	selected, err := source.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	other, err := source.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	for i, project := range []db.Project{selected, other} {
		_, err = source.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: []string{"01HZNQ7VFPK1XGD8R5MABCD4EA", "01HZNQ7VFPK1XGD8R5MABCD4EB"}[i], SpokeName: []string{"laptop", "build-box"}[i], Capabilities: "pull", Actor: "member"})
		require.NoError(t, err)
	}
	for _, legacy := range []bool{false, true} {
		mode := "current"
		if legacy {
			mode = "legacy"
		}
		t.Run(mode, func(t *testing.T) {
			var scoped bytes.Buffer
			opts := jsonl.ExportOptions{ProjectID: selected.ID}
			var exportStore db.Storage = source
			if legacy {
				exportStore = spokeNamesLegacyExportStore{source}
			}
			err = jsonl.Export(t.Context(), exportStore, &scoped, opts)
			require.NoError(t, err)
			records, err := jsonl.NewDecoder(bytes.NewReader(scoped.Bytes())).ReadAll(t.Context())
			require.NoError(t, err)
			for _, record := range records {
				if record.Kind == jsonl.KindMeta {
					var meta db.MetaKV
					require.NoError(t, json.Unmarshal(record.Data, &meta))
					require.False(t, strings.HasPrefix(meta.Key, db.FederationSpokeNamePrefix), "project exports must not carry hub-local names")
				}
			}
			for _, backend := range []string{"sqlite", "postgres"} {
				for _, merge := range []bool{false, true} {
					operation := "restore"
					if merge {
						operation = "merge"
					}
					t.Run(backend+"/"+operation, func(t *testing.T) {
						var target db.Storage
						if backend == "postgres" {
							dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
							t.Cleanup(cleanup)
							target, err = pgstore.Open(t.Context(), dsn)
						} else {
							target, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
						}
						require.NoError(t, err)
						t.Cleanup(func() { _ = target.Close() })
						if merge {
							_, err = target.CreateProject(t.Context(), "existing-project")
							require.NoError(t, err)
						}
						require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(scoped.Bytes()), target, jsonl.ImportOptions{MergeProject: merge}))
						restored, err := target.ProjectByUID(t.Context(), selected.UID)
						require.NoError(t, err)
						require.Equal(t, selected.Name, restored.Name)
						names, err := target.ListFederationSpokeNames(t.Context())
						require.NoError(t, err)
						require.Empty(t, names)
					})
				}
			}
		})
	}
}
