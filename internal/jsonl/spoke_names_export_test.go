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
	var markerKeys []string
	for _, project := range []db.Project{selected, other} {
		_, event, err := source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "export", Author: "worker"})
		require.NoError(t, err)
		require.NoError(t, source.RecordFederationAcknowledgedEvents(t.Context(), project.ID, []string{event.UID}))
		markerKeys = append(markerKeys, db.FederationAcknowledgedEventPrefix+project.UID+"."+event.UID)
	}
	_, err = source.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: selected.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: selected.ID, HubProjectUID: selected.UID, Actor: "worker", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	require.NoError(t, source.FenceFederationLeave(t.Context(), selected.ID))
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
						if !legacy && !merge {
							var markers []string
							for meta, err := range target.ExportMeta(t.Context()) {
								require.NoError(t, err)
								if strings.HasPrefix(meta.Key, db.FederationAcknowledgedEventPrefix) {
									markers = append(markers, meta.Key)
								}
								require.NotEqual(t, db.FederationLeaveFencePrefix+selected.UID, meta.Key)
							}
							require.Equal(t, []string{markerKeys[0]}, markers)
						}
					})
				}
			}
		})
	}
	var full bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source, &full, jsonl.ExportOptions{IncludeDeleted: true}))
	for _, standalone := range []bool{false, true} {
		name := "full"
		if standalone {
			name = "standalone"
		}
		t.Run(name, func(t *testing.T) {
			target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(full.Bytes()), target, jsonl.ImportOptions{AsStandalone: standalone}))
			var fenced bool
			for meta, err := range target.ExportMeta(t.Context()) {
				require.NoError(t, err)
				if meta.Key == db.FederationLeaveFencePrefix+selected.UID {
					fenced = true
				}
			}
			require.Equal(t, !standalone, fenced)
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
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	_, err = source.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	var full bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &full, jsonl.ExportOptions{IncludeDeleted: true}))
	target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(full.Bytes()), target, jsonl.ImportOptions{AsStandalone: true}))
	found := false
	for p, err := range target.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		if p.UID == project.UID {
			require.Nil(t, p.HeldCursor)
			found = true
		}
	}
	require.True(t, found)
}
