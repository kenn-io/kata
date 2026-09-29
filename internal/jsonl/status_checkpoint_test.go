package jsonl

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestStatusCheckpointExportAndRestore(t *testing.T) {
	for _, version := range []string{"29", "30", "31"} {
		t.Run(version, func(t *testing.T) {
			ctx := t.Context()
			source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = source.Close() })
			project, err := source.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			var mappings []db.ImportMapping
			var pendingUID string
			for i := range 3 {
				issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Mapped task", Author: "worker"})
				require.NoError(t, err)
				mapping, err := source.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: "github:example-source", ExternalID: []string{"issue-id:1", "issue-id:2", "issue-id:3"}[i], ObjectType: "issue", IssueID: &issue.ID})
				require.NoError(t, err)
				mappings = append(mappings, mapping)
				if i == 1 {
					_, event, _, err := source.CloseIssue(ctx, issue.ID, "done", "worker", "Completed task", nil)
					require.NoError(t, err)
					pendingUID = event.UID
				}
			}
			const stamp = "2026-09-29T12:00:00Z"
			switch version {
			case "30":
				for _, column := range []string{"observed_status", "observed_status_at", "pending_event_uid", "remote_locator"} {
					_, err = source.ExecContext(ctx, `ALTER TABLE import_mappings DROP COLUMN `+column)
					require.NoError(t, err)
				}
				_, err = source.ExecContext(ctx, `ALTER TABLE import_mappings ADD COLUMN status_sync_json TEXT`)
				require.NoError(t, err)
				legacy, err := json.Marshal(map[string]any{"observed": map[string]any{"raw": nil, "version": stamp}, "pending_event_uid": pendingUID, "github_issue_number": 7})
				require.NoError(t, err)
				_, err = source.ExecContext(ctx, `UPDATE import_mappings SET status_sync_json=? WHERE id=?`, string(legacy), mappings[1].ID)
				require.NoError(t, err)
				_, err = source.ExecContext(ctx, `UPDATE import_mappings SET status_sync_json=? WHERE id=?`, `{"observed":{"raw":"closed","version":"2026-09-29T12:00:00Z"}}`, mappings[2].ID)
				require.NoError(t, err)
			case "31":
				_, err = source.ExecContext(ctx, `UPDATE import_mappings SET observed_status_at=?,pending_event_uid=?,remote_locator='7' WHERE id=?`, stamp, pendingUID, mappings[1].ID)
				require.NoError(t, err)
				_, err = source.ExecContext(ctx, `UPDATE import_mappings SET observed_status='closed',observed_status_at=? WHERE id=?`, stamp, mappings[2].ID)
			}
			require.NoError(t, err)
			_, err = source.ExecContext(ctx, `UPDATE meta SET value=? WHERE key='schema_version'`, version)
			require.NoError(t, err)
			for _, legacyExporter := range []bool{false, true} {
				var output bytes.Buffer
				if legacyExporter {
					err = exportForCutover(ctx, source, &output, ExportOptions{})
				} else {
					err = Export(ctx, source, &output, ExportOptions{})
				}
				require.NoError(t, err)
				require.NotContains(t, output.String(), `"status_sync"`)
				require.NotContains(t, output.String(), `"github_issue_number"`)
				if version != "29" {
					require.Contains(t, output.String(), `"observed_status_at":"`+stamp+`"`)
					require.Contains(t, output.String(), `"pending_event_uid":"`+pendingUID+`"`)
					require.Contains(t, output.String(), `"remote_locator":"7"`)
				}
				for _, trusted := range []bool{false, true} {
					target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
					require.NoError(t, err)
					t.Cleanup(func() { _ = target.Close() })
					require.NoError(t, ImportWithOptions(ctx, bytes.NewReader(output.Bytes()), target, ImportOptions{PreserveIssueSyncBindingEnabled: trusted}))
					for i, mapping := range mappings {
						var raw, at, pending, locator sql.NullString
						require.NoError(t, target.QueryRowContext(ctx, `SELECT observed_status,CAST(observed_status_at AS TEXT),pending_event_uid,remote_locator FROM import_mappings WHERE id=?`, mapping.ID).Scan(&raw, &at, &pending, &locator))
						if !trusted || version == "29" || i == 0 {
							require.False(t, raw.Valid)
							require.False(t, at.Valid)
							require.False(t, pending.Valid)
							require.False(t, locator.Valid)
						} else {
							require.Equal(t, stamp, at.String)
							require.True(t, at.Valid)
							if i == 1 {
								require.False(t, raw.Valid, "observed null must retain timestamp presence")
								require.Equal(t, pendingUID, pending.String)
								require.Equal(t, "7", locator.String)
							} else {
								require.Equal(t, "closed", raw.String)
								require.False(t, pending.Valid)
								require.False(t, locator.Valid)
							}
						}
					}
				}
			}
		})
	}
}

func TestLegacyStatusCheckpointWireAndAutomaticCutover(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Mapped task", Author: "worker"})
	require.NoError(t, err)
	mapping, err := source.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: "notion:example-source", ExternalID: "11111111-1111-4111-8111-111111111111", ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	_, event, _, err := source.CloseIssue(ctx, issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	legacy, err := json.Marshal(map[string]any{"observed": map[string]any{"raw": nil, "version": "2026-09-29T12:00:00Z"}, "pending_event_uid": event.UID})
	require.NoError(t, err)
	for _, column := range []string{"observed_status", "observed_status_at", "pending_event_uid", "remote_locator"} {
		_, err = source.ExecContext(ctx, `ALTER TABLE import_mappings DROP COLUMN `+column)
		require.NoError(t, err)
	}
	_, err = source.ExecContext(ctx, `ALTER TABLE import_mappings ADD COLUMN status_sync_json TEXT`)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE import_mappings SET status_sync_json=? WHERE id=?`, string(legacy), mapping.ID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE meta SET value='30' WHERE key='schema_version'`)
	require.NoError(t, err)
	var portable bytes.Buffer
	require.NoError(t, Export(ctx, source, &portable, ExportOptions{}))
	records, err := NewDecoder(bytes.NewReader(portable.Bytes())).ReadAll(ctx)
	require.NoError(t, err)
	// Reconstruct an older wire record rather than only testing the normalized
	// exporter. Its legacy checkpoint is converted at the import boundary.
	for i, record := range records {
		if record.Kind == KindImportMapping {
			var mapping db.ImportMappingExport
			require.NoError(t, json.Unmarshal(record.Data, &mapping))
			mapping.ObservedStatus = nil
			mapping.ObservedStatusAt = nil
			mapping.PendingEventUID = nil
			mapping.RemoteLocator = nil
			mapping.StatusSync = jsontext.Value(legacy)
			records[i].Data, err = json.Marshal(mapping)
			require.NoError(t, err)
		}
	}
	var olderWire bytes.Buffer
	encoder := NewEncoder(&olderWire)
	for _, record := range records {
		require.NoError(t, encoder.Write(record))
	}
	for _, trusted := range []bool{false, true} {
		target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
		require.NoError(t, err)
		require.NoError(t, ImportWithOptions(ctx, bytes.NewReader(olderWire.Bytes()), target, ImportOptions{PreserveIssueSyncBindingEnabled: trusted}))
		assertConvertedNotionCheckpoint(t, target, mapping, event.UID, trusted)
		require.NoError(t, target.Close())
	}
	require.NoError(t, source.Close())
	require.NoError(t, AutoCutover(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	assertConvertedNotionCheckpoint(t, upgraded, mapping, event.UID, true)
}

func assertConvertedNotionCheckpoint(t *testing.T, store *sqlitestore.Store, mapping db.ImportMapping, pendingUID string, preserved bool) {
	t.Helper()
	var identity string
	var raw, at, pending, locator sql.NullString
	require.NoError(t, store.QueryRowContext(t.Context(), `SELECT external_id,observed_status,CAST(observed_status_at AS TEXT),pending_event_uid,remote_locator FROM import_mappings WHERE id=?`, mapping.ID).Scan(&identity, &raw, &at, &pending, &locator))
	require.Equal(t, mapping.ExternalID, identity)
	require.False(t, raw.Valid)
	require.False(t, locator.Valid, "Notion identity stays in external_id")
	require.Equal(t, preserved, at.Valid)
	require.Equal(t, preserved, pending.Valid)
	if preserved {
		require.Equal(t, "2026-09-29T12:00:00Z", at.String)
		require.Equal(t, pendingUID, pending.String)
	}
}
