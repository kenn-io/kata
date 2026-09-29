package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// This exercises the persisted contract, including generic mapping writes that
// must never transfer provider write authority to a different local issue.
func checkIssueStatusMappingStorage(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	fixture, err := createIssueFixture(ctx, store, "example-project", "Mapped task", "worker", nil)
	if err != nil {
		return err
	}
	p := db.ImportMappingParams{Source: "notion:example-source", ExternalID: "page-1", ObjectType: "issue", ProjectID: fixture.Project.ID, IssueID: &fixture.Issue.ID}
	mapping, err := store.UpsertImportMapping(ctx, p)
	if err != nil {
		return err
	}
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	read := func() db.ImportMappingExport {
		var state db.ImportMappingExport
		require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT observed_status,CAST(observed_status_at AS TEXT),pending_event_uid,remote_locator FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&state.ObservedStatus, &state.ObservedStatusAt, &state.PendingEventUID, &state.RemoteLocator))
		return state
	}
	require.Equal(t, db.ImportMappingExport{}, read(), "new mappings have no historical outbound intent")
	stamp, locator := "2026-09-29T12:00:00Z", "7"
	state := db.ImportMappingExport{ObservedStatusAt: &stamp, RemoteLocator: &locator}
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status_at=$1,remote_locator=$2 WHERE id=$3`, stamp, locator, mapping.ID)
	require.NoError(t, err)
	mapping, err = store.UpsertImportMapping(ctx, p)
	require.NoError(t, err)
	require.Equal(t, state, read(), "same-identity upsert retains the null observation and API locator")
	public, err := json.Marshal(mapping)
	require.NoError(t, err)
	require.NotContains(t, string(public), "status_sync")
	require.NotContains(t, string(public), "observed")
	var records []db.ImportRecord
	for record, err := range store.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for record, err := range store.ExportIssues(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for record, err := range store.ExportImportMappings(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		require.NotContains(t, fields, "status_sync")
		require.NotContains(t, fields, "github_issue_number")
		require.NotContains(t, fields, "observed_status", "null raw value is distinguished by timestamp presence")
		require.Equal(t, stamp, fields["observed_status_at"])
		require.Equal(t, locator, fields["remote_locator"])
		records = append(records, &record)
	}
	// The independent null-observation export above is followed by a fully
	// populated row to exercise preservation and clearing of all four columns.
	_, event, _, err := store.CloseIssue(ctx, fixture.Issue.ID, "done", "worker", "Completed mapped task", nil)
	require.NoError(t, err)
	rawStatus, pending := "closed", event.UID
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status=$1,pending_event_uid=$2 WHERE id=$3`, rawStatus, pending, mapping.ID)
	require.NoError(t, err)
	_, err = store.UpsertImportMapping(ctx, p)
	require.NoError(t, err)
	populated := state
	populated.ObservedStatus = &rawStatus
	populated.PendingEventUID = &pending
	require.Equal(t, populated, read(), "same-target upsert preserves all four columns")
	peer, err := createFixtureIssue(ctx, store, fixture.Project.ID, "Different task", "worker", nil)
	require.NoError(t, err)
	p.IssueID = &peer.ID
	_, err = store.UpsertImportMapping(ctx, p)
	require.NoError(t, err)
	require.Equal(t, db.ImportMappingExport{}, read(), "retargeting cannot transfer private status authority")
	for _, preserve := range []bool{false, true} {
		require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{PreserveIssueSyncBindingEnabled: preserve}))
		restored := read()
		if preserve {
			require.Equal(t, state, restored)
		} else {
			require.Equal(t, db.ImportMappingExport{}, restored)
		}
	}
	for _, record := range records {
		if checkpoint, ok := record.(*db.ImportMappingExport); ok {
			for _, invalid := range []string{`[]`, `{"observed":{"version":"2026-09-29T12:00:00Z"}}`, `{"pending_event_uid":"bad"}`, `{"desired_state":"closed"}`} {
				checkpoint.StatusSync = []byte(invalid)
				for _, trusted := range []bool{false, true} {
					require.Error(t, store.ImportReplay(ctx, records, db.ImportOptions{PreserveIssueSyncBindingEnabled: trusted}), "malformed checkpoint must be rejected even when it would be cleared")
					require.Equal(t, state, read(), "failed replay must retain the prior store")
				}
			}
		}
	}
	return nil
}
