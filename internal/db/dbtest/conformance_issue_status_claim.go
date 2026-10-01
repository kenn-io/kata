package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusClaimRecovery(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "example-project", "Mapped task", "worker", nil)
	require.NoError(t, err)
	params := db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way","title_prefix":true}`), IntervalSeconds: 60}
	binding, err := store.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "example-page", ObjectType: "issue", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	_, closeEvent, _, err := store.CloseIssue(ctx, fixture.Issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	base := fixture.Issue.CreatedAt.Add(time.Minute).UTC().Truncate(time.Millisecond)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", base, base.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	summary, ok := store.(interface {
		CountPendingIssueStatuses(context.Context, int64) (int, error)
	})
	require.True(t, ok, "both backends provide the operator's pending count")
	count, err := summary.CountPendingIssueStatuses(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: base}
	scans := store.(db.IssueStatusScanStore)
	scan := db.IssueStatusScanState{Pending: db.IssueStatusScanCursor{After: mapping.ID, Through: mapping.ID}, Sweep: db.IssueStatusScanCursor{Through: mapping.ID}, LocatorPage: 3}
	updated, err := scans.UpdateIssueStatusScan(ctx, guard, scan)
	require.NoError(t, err)
	readScan := func(config jsontext.Value) {
		actual, err := db.DecodeIssueStatusScan(config)
		require.NoError(t, err)
		require.Equal(t, scan, actual)
	}
	readScan(updated.Config)
	// Provider metadata refreshes carry only their typed user configuration.
	updated, err = store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{BindingID: binding.ID, StartedAt: &base, DisplayName: "Refreshed tasks", Config: params.Config})
	require.NoError(t, err)
	readScan(updated.Config)
	admittedGuard := guard
	admittedAt := updated.UpdatedAt
	admittedGuard.BindingUpdatedAt = &admittedAt
	_, err = store.DisableIssueSyncBinding(ctx, fixture.Project.ID)
	require.NoError(t, err)
	status, err := store.IssueSyncStatusByProject(ctx, fixture.Project.ID)
	require.NoError(t, err)
	require.Equal(t, &base, status.SyncStartedAt, "disable must retain the retiring writer's claim")
	params.Config = jsontext.Value(`{"status_sync":"two-way","title_prefix":false}`)
	updated, err = store.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	readScan(updated.Config)
	_, err = store.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{BindingID: binding.ID, StartedAt: base, At: base, CursorAt: base, BindingUpdatedAt: &admittedAt})
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged, "superseded config cannot advance the content cursor")
	_, err = store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, admittedGuard, mapping.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged, "retaining a claim must not authorize the old binding snapshot")
	_, claimed, err = store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", base.Add(time.Minute), base.Add(-time.Hour))
	require.NoError(t, err)
	require.False(t, claimed, "re-enable/config edits cannot admit a replacement writer")
	status, err = store.RecordIssueSyncError(ctx, db.IssueSyncErrorParams{BindingID: binding.ID, StartedAt: base, At: base.Add(time.Minute), Error: "Provider write outcome is ambiguous", RetainClaim: true})
	require.NoError(t, err)
	require.Equal(t, &base, status.SyncStartedAt)
	// Returning to one-way cancels pending intent, but still keeps the retiring
	// writer fenced until its conservative recovery horizon has elapsed.
	params.Config = jsontext.Value(`{"status_sync":"one-way","title_prefix":false}`)
	_, err = store.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	current, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Empty(t, current.State.PendingEventUID)
	require.NotEmpty(t, closeEvent.UID)
	_, claimed, err = store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", base.Add(29*time.Minute), base.Add(-time.Minute))
	require.NoError(t, err)
	require.False(t, claimed)
	_, claimed, err = store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", base.Add(31*time.Minute), base.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = scans.UpdateIssueStatusScan(ctx, guard, scan)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	// Ordinary restore loses host-local scan progress, trusted cutover retains it.
	var records []db.ImportRecord
	for record, err := range store.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for record, err := range store.ExportIssueSyncBindings(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for record, err := range store.ExportIssueSyncStatus(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for _, trusted := range []bool{false, true} {
		require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{PreserveIssueSyncBindingEnabled: trusted}))
		restored, err := store.IssueSyncBindingByID(ctx, binding.ID)
		require.NoError(t, err)
		actual, err := db.DecodeIssueStatusScan(restored.Config)
		require.NoError(t, err)
		restoredStatus, err := store.IssueSyncStatusByProject(ctx, fixture.Project.ID)
		require.NoError(t, err)
		if trusted {
			require.Equal(t, new(base.Add(31*time.Minute)), restoredStatus.SyncStartedAt)
		} else {
			require.Nil(t, restoredStatus.SyncStartedAt)
		}
		if trusted {
			require.Equal(t, scan, actual)
		} else {
			require.Equal(t, db.IssueStatusScanState{}, actual)
			require.False(t, restored.Enabled)
		}
	}
	return nil
}
