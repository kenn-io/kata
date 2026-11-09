package dbtest

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkTrackerRecoveryReceipt(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "recovery-project", "Mapped task", "member", nil)
	require.NoError(t, err)
	selection := db.IssueSyncConnection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first"}
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "page:item", ObjectType: "issue", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	epoch := "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	_, err = store.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, epoch)
	require.NoError(t, err)
	_, events, _, err := store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "member", "Completed task", nil)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", now, now.Add(-30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	p := db.TrackerStatusAdmission{Guard: db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: now, BindingUpdatedAt: &binding.UpdatedAt}, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: fixture.Issue.UID, PendingEventUID: events[0].UID, Target: "closed", Connection: selection, ProcessEpoch: epoch, Now: now, RemoteAttempt: &db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", ArgumentsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", OwnerSequence: 1, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2"}}
	admissions := store.(db.TrackerStatusStore)
	prepared, err := admissions.AdmitTrackerStatusWrite(ctx, p)
	require.NoError(t, err)
	p.ExpectedReservation = &prepared
	sent, err := admissions.AdmitTrackerStatusWrite(ctx, p)
	require.NoError(t, err)
	proven := sent
	proven.Phase = db.TrackerWriteConfirmed
	proven.ProviderAcknowledged = true
	proven.Result, err = json.Marshal(db.TrackerStatusResult{Status: "closed", Version: now})
	require.NoError(t, err)
	fences := store.(db.TrackerFenceStore)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &sent, proven, epoch))
	_, newer, changed, err := store.ReopenIssue(ctx, fixture.Issue.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	_, err = store.DisableIssueSyncBinding(ctx, fixture.Project.ID)
	require.NoError(t, err)
	recovery := store.(db.TrackerRecoveryReceiptStore)
	require.NoError(t, recovery.AcknowledgeTrackerRecovery(ctx, fixture.Project.UID))
	require.NoError(t, recovery.AcknowledgeTrackerRecovery(ctx, fixture.Project.UID))
	head, _, err := fences.LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.Equal(t, db.TrackerWriteAcknowledged, head.Phase)
	cp, found, err := store.(db.TrackerOwnerCheckpointStore).LoadTrackerOwnerCheckpoint(ctx, store.InstanceUID(), fixture.Project.UID, selection.OwnerInstanceUID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, uint64(1), cp.Sequence)
	current, err := store.ImportMappingBySource(ctx, fixture.Project.ID, binding.SourceKey, "issue", mapping.ExternalID)
	require.NoError(t, err)
	require.NotNil(t, current.IssueID)
	issue, err := store.IssueByID(ctx, *current.IssueID)
	require.NoError(t, err)
	require.Equal(t, "open", issue.Status)
	foundPending := false
	for exported, err := range store.ExportImportMappings(ctx, db.ExportFilter{ProjectID: &fixture.Project.ID}) {
		require.NoError(t, err)
		if exported.ID == mapping.ID {
			foundPending = true
			require.NotNil(t, exported.PendingEventUID)
			require.Equal(t, newer.UID, *exported.PendingEventUID)
			require.Nil(t, exported.ObservedStatus, "metadata acknowledgement does not invent a provider observation")
		}
	}
	require.True(t, foundPending)
	history, found, err := store.(db.TrackerFenceHistoryStore).LoadTrackerExecutionHistory(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator", head.WriteUID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, head.WriteUID, history.WriteUID)
	proofs, err := store.(db.TrackerRecoveryHistoryLister).ListTrackerRecoveryHistory(ctx, store.InstanceUID(), fixture.Project.UID, "", 1)
	require.NoError(t, err)
	require.Equal(t, []db.TrackerExecutionFence{history}, proofs)
	proofs, err = store.(db.TrackerRecoveryHistoryLister).ListTrackerRecoveryHistory(ctx, store.InstanceUID(), fixture.Project.UID, history.WriteUID, 1)
	require.NoError(t, err)
	require.Empty(t, proofs)
	// Retained owner discovery is independent of the current binding/config.
	// Paging releases its database transaction before any remote recovery I/O.
	lister := store.(db.TrackerOwnerCheckpointLister)
	page, err := lister.ListTrackerOwnerCheckpoints(ctx, store.InstanceUID(), fixture.Project.UID, "", 1)
	require.NoError(t, err)
	require.Equal(t, []db.TrackerOwnerCheckpoint{cp}, page)
	page, err = lister.ListTrackerOwnerCheckpoints(ctx, store.InstanceUID(), fixture.Project.UID, cp.OwnerInstanceUID, 1)
	require.NoError(t, err)
	require.Empty(t, page)
	_, err = lister.ListTrackerOwnerCheckpoints(db.WithAuthorizedProjects(ctx, []string{}), store.InstanceUID(), fixture.Project.UID, "", 128)
	require.ErrorIs(t, err, db.ErrNotFound)
	return nil
}
