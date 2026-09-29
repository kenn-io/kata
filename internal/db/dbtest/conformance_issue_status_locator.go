package dbtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusLocator(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "Mapped task", "worker", nil)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: f.Project.ID, Provider: "github", SourceKey: "github:example-repository", RemoteID: "example-repository", DisplayName: "example-owner/example-repo", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	m, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: f.Project.ID, Source: b.SourceKey, ObjectType: "issue", ExternalID: "issue:example-node", IssueID: &f.Issue.ID})
	require.NoError(t, err)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status_at=$1 WHERE id=$2`, at.Format(time.RFC3339Nano), m.ID)
	require.NoError(t, err)
	_, event, _, err := store.CloseIssue(ctx, f.Issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	_, ok, err := store.ClaimIssueSyncBinding(ctx, b.ID, "github", at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	locators, ok := store.(interface {
		SaveIssueStatusLocator(context.Context, db.IssueSyncImportGuard, db.IssueStatusLocator) (bool, error)
	})
	require.True(t, ok, "verified API identifiers use an independent guarded write")
	guard := db.IssueSyncImportGuard{BindingID: b.ID, Provider: "github", StartedAt: at, BindingUpdatedAt: new(b.UpdatedAt)}
	locator := db.IssueStatusLocator{ExternalID: "issue-id:123", LegacyExternalIDs: []string{"issue:example-node"}, Locator: "7"}
	inactiveIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.Project.ID, Title: "Inactive mapped task", Author: "worker"})
	require.NoError(t, err)
	inactiveMapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: f.Project.ID, Source: b.SourceKey, ObjectType: "issue", ExternalID: "issue:inactive-node", IssueID: &inactiveIssue.ID})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, inactiveIssue.ID, "worker")
	require.NoError(t, err)
	inactiveLocator := db.IssueStatusLocator{ExternalID: "issue-id:456", LegacyExternalIDs: []string{inactiveMapping.ExternalID}, Locator: "8"}
	inactiveSaved, err := locators.SaveIssueStatusLocator(ctx, guard, inactiveLocator)
	require.NoError(t, err)
	require.False(t, inactiveSaved, "inactive local mappings do not receive provider locators")
	saved, err := locators.SaveIssueStatusLocator(ctx, guard, locator)
	require.NoError(t, err)
	require.True(t, saved)
	current, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, guard, m.ID)
	require.NoError(t, err)
	require.Equal(t, m.ID, current.Mapping.ID)
	require.Equal(t, locator.ExternalID, current.Mapping.ExternalID)
	require.Equal(t, "7", current.State.RemoteLocator)
	require.NotNil(t, current.State.Observed)
	require.Nil(t, current.State.Observed.Raw)
	require.Equal(t, event.UID, current.State.PendingEventUID)
	failSQL := `CREATE TRIGGER fail_status_locator_test BEFORE UPDATE OF remote_locator ON import_mappings BEGIN SELECT RAISE(FAIL, 'injected locator write failure'); END`
	dropSQL := `DROP TRIGGER fail_status_locator_test`
	if backend.Name == "postgres" {
		failSQL = `CREATE FUNCTION fail_status_locator_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected locator write failure'; END $$; CREATE TRIGGER fail_status_locator_test BEFORE UPDATE OF remote_locator ON import_mappings FOR EACH ROW EXECUTE FUNCTION fail_status_locator_test()`
		dropSQL = `DROP TRIGGER fail_status_locator_test ON import_mappings; DROP FUNCTION fail_status_locator_test()`
	}
	_, err = sqlStore.ExecContext(ctx, failSQL)
	require.NoError(t, err)
	saved, err = locators.SaveIssueStatusLocator(ctx, guard, locator)
	require.NoError(t, err, "an unchanged verified locator must not issue an UPDATE")
	require.True(t, saved)
	changedLocator := locator
	changedLocator.Locator = "8"
	_, err = locators.SaveIssueStatusLocator(ctx, guard, changedLocator)
	require.Error(t, err, "the test guard must reject an actual locator UPDATE")
	_, err = sqlStore.ExecContext(ctx, dropSQL)
	require.NoError(t, err)
	current, err = store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, guard, m.ID)
	require.NoError(t, err)
	require.Equal(t, "7", current.State.RemoteLocator)
	require.Equal(t, event.UID, current.State.PendingEventUID)
	locator.ExternalID = "issue-id:456"
	locator.LegacyExternalIDs = nil
	saved, err = locators.SaveIssueStatusLocator(ctx, guard, locator)
	require.NoError(t, err)
	require.False(t, saved, "enumeration cannot create an import outside the content cutoff")
	guard.StartedAt = at.Add(time.Second)
	_, err = locators.SaveIssueStatusLocator(ctx, guard, locator)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	return nil
}
