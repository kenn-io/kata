package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusObservation(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "example-project", "Original title", "worker", nil)
	require.NoError(t, err)
	title := "Locally edited title"
	issue, _, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: fixture.Issue.ID, Title: &title, Actor: "worker"})
	require.NoError(t, err)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var priorContentRevision int64
	require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT content_revision FROM issues WHERE id=$1`, issue.ID).Scan(&priorContentRevision))
	base := issue.CreatedAt.Add(time.Minute).UTC()
	_, err = sqlStore.ExecContext(ctx, `UPDATE issues SET updated_at=$1 WHERE id=$2`, base.Add(10*time.Minute).Format(time.RFC3339Nano), issue.ID)
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "example-page", ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", base, base.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: base}
	writer := store.(db.IssueStatusWriter)
	reader := store.(db.IssueStatusReader)
	complete, todo := "complete-a", "todo-a"
	params := db.IssueStatusObservationParams{Guard: guard, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: issue.UID, Observation: db.IssueStatusObservation{Raw: &complete, Version: base}, Status: "closed"}
	changed, events, err := writer.ObserveIssueStatus(ctx, params)
	require.NoError(t, err)
	require.True(t, changed, "provider status authority is independent of a newer local title")
	require.Len(t, events, 1)
	require.Equal(t, "issue.updated", events[0].Type)
	require.Equal(t, "notion-sync", events[0].Actor)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	require.Equal(t, "closed", payload["status"])
	require.NotContains(t, payload, "title")
	require.NotContains(t, payload, "body")
	closed, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, title, closed.Title)
	require.Equal(t, "closed", closed.Status)
	var revision int64
	require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT content_revision FROM issues WHERE id=$1`, issue.ID).Scan(&revision))
	require.Equal(t, priorContentRevision, revision, "status does not alter the content revision")
	current, err := reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Empty(t, current.State.PendingEventUID, "inward observations must not enqueue outbound echo")
	params.Observation.Version = base.Add(time.Minute)
	changed, events, err = writer.ObserveIssueStatus(ctx, params)
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, events)
	same, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, closed.ClosedAt, same.ClosedAt)
	require.Equal(t, closed.ClosedReason, same.ClosedReason)
	require.Equal(t, closed.Revision, same.Revision)
	// Old observations cannot regress the accepted provider checkpoint.
	params.Status = "open"
	params.Observation = db.IssueStatusObservation{Raw: &todo, Version: base}
	changed, events, err = writer.ObserveIssueStatus(ctx, params)
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, events)
	current, err = reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, complete, *current.State.Observed.Raw)
	// A live group reclassification can change the binary state without a
	// newer raw option or provider edit timestamp, after an authoritative read.
	params.Observation = *current.State.Observed
	changed, events, err = writer.ObserveIssueStatus(ctx, params)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 1)
	_, oldClose, _, err := store.CloseIssue(ctx, issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	nativeClosed, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	params.Status = "closed"
	params.Observation = db.IssueStatusObservation{Raw: &complete, Version: base.Add(2 * time.Minute)}
	params.ServicedEventUID = oldClose.UID
	changed, events, err = writer.ObserveIssueStatus(ctx, params)
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, events)
	acknowledged, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, nativeClosed.ClosedAt, acknowledged.ClosedAt)
	require.Equal(t, nativeClosed.ClosedReason, acknowledged.ClosedReason)
	require.Equal(t, nativeClosed.Revision, acknowledged.Revision)
	current, err = reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Empty(t, current.State.PendingEventUID)
	_, newReopen, _, err := store.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	// A verified old close acknowledgement observes its readback but cannot
	// clear a newer reopen or overwrite its current native status.
	params.Status = "closed"
	params.Observation = db.IssueStatusObservation{Raw: &complete, Version: base.Add(2 * time.Minute)}
	params.ServicedEventUID = oldClose.UID
	changed, events, err = writer.ObserveIssueStatus(ctx, params)
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, events)
	current, err = reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, newReopen.UID, current.State.PendingEventUID)
	open, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, "open", open.Status)
	params.ServicedEventUID = newReopen.UID
	_, _, err = writer.ObserveIssueStatus(ctx, params)
	require.ErrorIs(t, err, db.ErrImportValidation, "cannot acknowledge an open intent with closed readback")
	params.Status = "open"
	params.Observation = db.IssueStatusObservation{Raw: &todo, Version: base.Add(3 * time.Minute)}
	params.ServicedEventUID = newReopen.UID
	changed, events, err = writer.ObserveIssueStatus(ctx, params)
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, events)
	current, err = reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Empty(t, current.State.PendingEventUID)
	require.Equal(t, todo, *current.State.Observed.Raw)
	params.Guard.StartedAt = base.Add(time.Second)
	_, _, err = writer.ObserveIssueStatus(ctx, params)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	params.Guard = guard
	params.ExternalID = "another-page"
	_, _, err = writer.ObserveIssueStatus(ctx, params)
	require.ErrorIs(t, err, db.ErrImportValidation)
	// Observation, native projection, and returned event share a transaction.
	params.ExternalID = mapping.ExternalID
	params.Status = "closed"
	params.ServicedEventUID = ""
	params.Observation.Version = base.Add(4 * time.Minute)
	before, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	checkpoint, err := reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	high, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	failSQL := `CREATE TRIGGER fail_status_observation_test BEFORE INSERT ON events BEGIN SELECT RAISE(FAIL,'injected event failure'); END`
	dropSQL := `DROP TRIGGER fail_status_observation_test`
	if backend.Name == "postgres" {
		failSQL = `CREATE FUNCTION fail_status_observation_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected event failure'; END $$; CREATE TRIGGER fail_status_observation_test BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_status_observation_test()`
		dropSQL = `DROP TRIGGER fail_status_observation_test ON events; DROP FUNCTION fail_status_observation_test()`
	}
	_, err = sqlStore.ExecContext(ctx, failSQL)
	require.NoError(t, err)
	_, _, err = writer.ObserveIssueStatus(ctx, params)
	require.Error(t, err)
	_, err = sqlStore.ExecContext(ctx, dropSQL)
	require.NoError(t, err)
	after, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	retained, err := reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, checkpoint.State, retained.State)
	afterHigh, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, high, afterHigh)
	return nil
}
