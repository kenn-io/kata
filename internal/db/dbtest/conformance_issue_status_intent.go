package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusNativeIntent(t *testing.T, store db.Storage, backend Backend) error {
	ctx := context.Background()
	sqlStore := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for _, provider := range []string{"notion", "github", "plane", "linear", "twenty", "todoist"} {
		for _, mode := range []string{"", "one-way", "two-way", "paused"} {
			fixture, err := createIssueFixture(ctx, store, fmt.Sprintf("example-%s-%s", provider, mode), "Mapped task", "worker", nil)
			require.NoError(t, err)
			config := `{}`
			if mode != "" {
				selected := mode
				if mode == "paused" {
					selected = "two-way"
				}
				config = `{"status_sync":"` + selected + `"}`
			}
			binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: provider, SourceKey: provider + ":example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(config), IntervalSeconds: 60})
			require.NoError(t, err)
			if mode == "paused" {
				_, err = store.DisableIssueSyncBinding(ctx, fixture.Project.ID)
				require.NoError(t, err)
			}
			mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: fixture.Issue.UID, ObjectType: "issue", IssueID: &fixture.Issue.ID})
			require.NoError(t, err)
			_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status=$1, observed_status_at=$2, remote_locator=$3 WHERE id=$4`, "status-option", "2026-09-29T12:00:00Z", "remote-17", mapping.ID)
			require.NoError(t, err)
			read := func() string {
				var raw, observedAt, pending, locator *string
				require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT observed_status, CAST(observed_status_at AS TEXT), pending_event_uid, remote_locator FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&raw, &observedAt, &pending, &locator))
				require.NotNil(t, raw)
				require.Equal(t, "status-option", *raw, "native intent preserves the provider observation")
				require.NotNil(t, observedAt)
				require.Equal(t, "2026-09-29T12:00:00Z", *observedAt)
				require.NotNil(t, locator)
				require.Equal(t, "remote-17", *locator, "native intent preserves the remote locator")
				if pending == nil {
					return ""
				}
				return *pending
			}
			_, events, changed, err := store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "worker", "Completed task", nil)
			require.NoError(t, err)
			require.True(t, changed)
			state := read()
			if mode == "two-way" || mode == "paused" {
				require.Equal(t, events[0].UID, state)
			} else {
				require.Empty(t, state)
			}
			_, _, changed, err = store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "worker", "Already complete", nil)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, state, read())
			_, event, changed, err := store.ReopenIssue(ctx, fixture.Issue.ID, "worker")
			require.NoError(t, err)
			require.True(t, changed)
			state = read()
			if mode == "two-way" || mode == "paused" {
				require.Equal(t, event.UID, state)
			} else {
				require.Empty(t, state)
			}
			_, _, changed, err = store.ReopenIssue(ctx, fixture.Issue.ID, "worker")
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, state, read())
			if provider == "notion" && mode == "two-way" {
				before, err := store.IssueByID(ctx, fixture.Issue.ID)
				require.NoError(t, err)
				eventID, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				failSQL := `CREATE TRIGGER fail_status_intent_test BEFORE UPDATE OF pending_event_uid ON import_mappings BEGIN SELECT RAISE(FAIL, 'injected checkpoint write failure'); END`
				dropSQL := `DROP TRIGGER fail_status_intent_test`
				if backend.Name == "postgres" {
					failSQL = `CREATE FUNCTION fail_status_intent_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected checkpoint write failure'; END $$; CREATE TRIGGER fail_status_intent_test BEFORE UPDATE OF pending_event_uid ON import_mappings FOR EACH ROW EXECUTE FUNCTION fail_status_intent_test()`
					dropSQL = `DROP TRIGGER fail_status_intent_test ON import_mappings; DROP FUNCTION fail_status_intent_test()`
				}
				_, err = sqlStore.ExecContext(ctx, failSQL)
				require.NoError(t, err)
				_, _, _, err = store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "worker", "Must roll back", nil)
				require.Error(t, err, "checkpoint write failure must roll back native issue and event")
				_, cleanupErr := sqlStore.ExecContext(ctx, dropSQL)
				require.NoError(t, cleanupErr)
				after, err := store.IssueByID(ctx, fixture.Issue.ID)
				require.NoError(t, err)
				require.Equal(t, before.Status, after.Status)
				require.Equal(t, before.Revision, after.Revision)
				afterEvent, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, eventID, afterEvent)
				require.Equal(t, state, read())
			}
		}
	}
	return nil
}
