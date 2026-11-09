package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A full backup taken before any named binding cannot prove that a later
// provider dispatch never occurred. Fresh bindings/generations cannot escape
// that missing history. Trusted current schema cutover is a separate boundary.
func checkTrackerRestoreWithoutJournal(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "tracker-restore-project")
	require.NoError(t, err)
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	for _, trustedCutover := range []bool{false, true} {
		target := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, target.Close()) })
		require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{PreserveIssueSyncBindingEnabled: trustedCutover}))
		restored, err := target.ProjectByUID(ctx, project.UID)
		require.NoError(t, err)
		selection := db.IssueSyncConnection{OwnerInstanceUID: target.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
		params := db.UpsertIssueSyncBindingParams{ProjectID: restored.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection}
		binding, err := target.UpsertIssueSyncBinding(ctx, params)
		require.NoError(t, err)
		const epoch = "01ARZ3NDEKTSV4RRFFQ69G5FB0"
		_, err = target.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, epoch)
		if trustedCutover {
			require.NoError(t, err)
			continue
		}
		require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown)
		successor := selection
		successor.Generation = "second"
		params.Connection = &successor
		params.ExpectedBinding = &db.IssueSyncBindingPrecondition{ID: binding.ID, Config: binding.Config, IntervalSeconds: binding.IntervalSeconds}
		rebound, err := target.(db.TrackerRebindStore).RebindTrackerConnection(ctx, db.TrackerRebindParams{Binding: params, ExpectedUpdatedAt: binding.UpdatedAt, ProcessEpoch: epoch})
		require.NoError(t, err, "read-only configuration may be replaced without claiming missing history is resolved")
		_, err = target.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, rebound.ID, rebound.UpdatedAt, epoch)
		require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "rebind must retain the restore-history barrier")
	}
	return nil
}

func checkTrackerProjectImportHistoryScope(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "tracker-imported-project")
	require.NoError(t, err)
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{ProjectID: &project.ID})
	require.NoError(t, err)
	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	unrelated, err := target.CreateProject(ctx, "tracker-unrelated-project")
	require.NoError(t, err)
	selection := db.IssueSyncConnection{OwnerInstanceUID: target.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
	params := db.UpsertIssueSyncBindingParams{ProjectID: unrelated.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection}
	binding, err := target.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	const epoch = "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	_, err = target.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, epoch)
	require.NoError(t, err)
	require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}))
	active, found, err := target.(db.TrackerActivationStore).LoadTrackerActivation(ctx, unrelated.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, active.Active)
	require.False(t, active.RestoredHistoryGap)
	blocked, err := target.(db.TrackerRestoreStateStore).TrackerRestoreHistoryBlocked(ctx, unrelated.UID)
	require.NoError(t, err)
	require.False(t, blocked, "one project import does not suspend an unrelated target's execution")
	imported, err := target.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	params.ProjectID = imported.ID
	params.SourceKey = "notion:imported-source"
	params.RemoteID = "imported-source"
	binding, err = target.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	_, err = target.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, epoch)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "imported project can predate every binding and journal row")
	return nil
}
