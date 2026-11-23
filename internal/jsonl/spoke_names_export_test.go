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

func TestFederationHeldCursorBackup(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "held-project")
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO federation_held_cursors(project_uid,push_cursor_event_id) VALUES(?,0)`, project.UID)
	require.NoError(t, err)
	var full, scoped bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &full, jsonl.ExportOptions{IncludeDeleted: true}))
	require.NoError(t, jsonl.Export(ctx, source, &scoped, jsonl.ExportOptions{ProjectID: project.ID, IncludeDeleted: true}))
	for _, tc := range []struct {
		name  string
		input []byte
		opts  jsonl.ImportOptions
		want  bool
	}{
		{"full", full.Bytes(), jsonl.ImportOptions{}, true},
		{"standalone", full.Bytes(), jsonl.ImportOptions{AsStandalone: true}, false},
		{"project", scoped.Bytes(), jsonl.ImportOptions{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(tc.input), target, tc.opts))
			var got *int64
			for p, err := range target.ExportProjects(ctx, db.ExportFilter{}) {
				require.NoError(t, err)
				if p.UID == project.UID {
					got = p.HeldCursor
				}
			}
			if tc.want {
				require.NotNil(t, got)
				require.Zero(t, *got)
			} else {
				require.Nil(t, got)
			}
		})
	}
}

func TestFederationAcknowledgmentProjectExport(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	selected, err := source.CreateProject(ctx, "selected-project")
	require.NoError(t, err)
	other, err := source.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	for _, project := range []db.Project{selected, other} {
		_, err := source.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?, '1')`, db.FederationAcknowledgedEventPrefix+project.UID+".event")
		require.NoError(t, err)
	}
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{ProjectID: selected.ID, IncludeDeleted: true}))
	target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(backup.Bytes()), target, jsonl.ImportOptions{}))
	var found []string
	for meta, err := range target.ExportMeta(ctx) {
		require.NoError(t, err)
		if strings.HasPrefix(meta.Key, db.FederationAcknowledgedEventPrefix) {
			found = append(found, meta.Key)
		}
	}
	require.Equal(t, []string{db.FederationAcknowledgedEventPrefix + selected.UID + ".event"}, found)
}

func TestFederationLeaveFenceBackup(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "prepared-project")
	require.NoError(t, err)
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	require.NoError(t, source.FenceFederationLeave(ctx, project.ID))
	var full, scoped bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &full, jsonl.ExportOptions{IncludeDeleted: true}))
	require.NoError(t, jsonl.Export(ctx, source, &scoped, jsonl.ExportOptions{ProjectID: project.ID, IncludeDeleted: true}))
	for _, tc := range []struct {
		name  string
		input []byte
		opts  jsonl.ImportOptions
		want  bool
	}{
		{"full", full.Bytes(), jsonl.ImportOptions{}, true},
		{"standalone", full.Bytes(), jsonl.ImportOptions{AsStandalone: true}, false},
		{"project", scoped.Bytes(), jsonl.ImportOptions{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(tc.input), target, tc.opts))
			var fenced bool
			for meta, err := range target.ExportMeta(ctx) {
				require.NoError(t, err)
				if meta.Key == db.FederationLeaveFencePrefix+project.UID {
					fenced = true
				}
			}
			require.Equal(t, tc.want, fenced)
			if tc.want {
				restored, err := target.ProjectByUID(ctx, project.UID)
				require.NoError(t, err)
				binding, err := target.FederationBindingByProject(ctx, restored.ID)
				require.NoError(t, err)
				require.False(t, binding.PushEnabled)
			}
		})
	}
}
