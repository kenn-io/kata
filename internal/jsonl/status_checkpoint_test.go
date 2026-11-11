package jsonl

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestStatusCheckpointExportAndRestore(t *testing.T) {
	for _, version := range []string{"29", "30"} {
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
