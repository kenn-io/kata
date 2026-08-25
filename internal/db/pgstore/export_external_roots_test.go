package pgstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	_ "modernc.org/sqlite"
)

func TestExportExternalFieldStatesReturnsPortableMappingIdentity(t *testing.T) {
	store := openPortableExportQueryTestStore(t)
	raw := store.DB

	_, err := raw.ExecContext(context.Background(), `
INSERT INTO issues(id) VALUES (1);
INSERT INTO external_root_bindings(id, uid, issue_id, project_id, connector_instance)
VALUES (1, 'binding-uid', 1, 1, 'notes');
INSERT INTO external_field_mappings(
    id, connector_instance, kata_field, external_field_id, external_field_name,
    accepted_kinds_json, nullable, writable, schema_revision, active, created_at, updated_at
) VALUES (
    1, 'notes', 'scheduled_on', 'due', 'Due', '["date"]', 1, 1,
    'v1', 1, '2026-08-20T06:00:00Z', '2026-08-20T06:00:00Z'
);
INSERT INTO external_field_states(
    binding_id, mapping_id, baseline_json, conflict_kata, conflict_external,
    conflicted, conflict_at, updated_at
) VALUES (1, 1, '"2026-08-21"', NULL, NULL, 0, NULL, '2026-08-20T06:01:00Z');
`)
	require.NoError(t, err)

	records := collectPGExport(t, store.ExportExternalFieldStates(t.Context(), db.ExportFilter{}))
	require.Len(t, records, 1)
	assert.Equal(t, "binding-uid", records[0].BindingUID)
	assert.Equal(t, "notes", records[0].MappingConnectorInstance)
	assert.Equal(t, "due", records[0].MappingExternalFieldID)
}

func TestExportExternalFieldMappingsIncludesActiveMappingsBeforeStateExists(t *testing.T) {
	store := openPortableExportQueryTestStore(t)
	raw := store.DB
	_, err := raw.ExecContext(context.Background(), `
INSERT INTO issues(id) VALUES (1);
INSERT INTO external_root_bindings(id, uid, issue_id, project_id, connector_instance)
VALUES (1, 'binding-uid', 1, 7, 'notes');
INSERT INTO external_field_mappings(
    id, connector_instance, kata_field, external_field_id, external_field_name,
    accepted_kinds_json, nullable, writable, schema_revision, active, created_at, updated_at
) VALUES
    (1, 'notes', 'scheduled_on', 'due-v1', 'Due', '["date"]', 1, 1,
     'v1', 0, '2026-08-20T06:00:00Z', '2026-08-20T06:00:00Z'),
    (2, 'notes', 'scheduled_on', 'due-v2', 'Due', '["date"]', 1, 1,
     'v2', 1, '2026-08-20T06:01:00Z', '2026-08-20T06:01:00Z'),
    (3, 'unbound', 'scheduled_on', 'other', 'Other', '["date"]', 1, 1,
     'v1', 1, '2026-08-20T06:02:00Z', '2026-08-20T06:02:00Z');
INSERT INTO external_field_states(
    binding_id, mapping_id, baseline_json, conflict_kata, conflict_external,
    conflicted, conflict_at, updated_at
) VALUES (1, 1, '"2026-08-21"', NULL, NULL, 0, NULL, '2026-08-20T06:03:00Z');
`)
	require.NoError(t, err)

	projectID := int64(7)
	records := collectPGExport(t, store.ExportExternalFieldMappings(
		t.Context(), db.ExportFilter{ProjectID: &projectID},
	))
	require.Len(t, records, 2)
	assert.Equal(t, "due-v1", records[0].ExternalFieldID)
	assert.Equal(t, "due-v2", records[1].ExternalFieldID)
}

func openPortableExportQueryTestStore(t *testing.T) *Store {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })

	_, err = raw.ExecContext(context.Background(), `
CREATE TABLE issues (id INTEGER PRIMARY KEY, deleted_at TEXT);
CREATE TABLE external_root_bindings (
    id INTEGER PRIMARY KEY, uid TEXT NOT NULL, issue_id INTEGER NOT NULL,
    project_id INTEGER NOT NULL, connector_instance TEXT NOT NULL
);
CREATE TABLE external_field_mappings (
    id INTEGER PRIMARY KEY, connector_instance TEXT NOT NULL,
    kata_field TEXT NOT NULL, external_field_id TEXT NOT NULL, external_field_name TEXT NOT NULL,
    accepted_kinds_json TEXT NOT NULL, nullable INTEGER NOT NULL, writable INTEGER NOT NULL,
    schema_revision TEXT NOT NULL, active INTEGER NOT NULL,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE external_field_states (
    binding_id INTEGER NOT NULL, mapping_id INTEGER NOT NULL,
    baseline_json TEXT, conflict_kata TEXT, conflict_external TEXT,
    conflicted INTEGER NOT NULL, conflict_at TEXT, updated_at TEXT NOT NULL
);
`)
	require.NoError(t, err)
	return &Store{DB: raw}
}
