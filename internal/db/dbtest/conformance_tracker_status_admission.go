package dbtest

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"testing"
	"time"
)

// Contract: current binding, claim, exact event and activation are checked in
// the same transaction as durable dispatch. Reclaiming a run cannot resend it.
func checkTrackerStatusAdmission(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "tracker-admission-project", "Mapped task", "member", nil)
	require.NoError(t, err)
	selection := db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "page:22222222-2222-4222-8222-222222222222", ObjectType: "issue", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	epoch := "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	_, err = store.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, epoch)
	require.NoError(t, err)
	_, events, changed, err := store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "member", "Completed task", nil)
	require.NoError(t, err)
	require.True(t, changed)
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", at, at.Add(-30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	p := db.TrackerStatusAdmission{Guard: db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: at, BindingUpdatedAt: &binding.UpdatedAt}, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: fixture.Issue.UID, PendingEventUID: events[0].UID, Target: "closed", Connection: selection, ProcessEpoch: epoch, Now: at}
	admissions := store.(db.TrackerStatusStore)
	bad := p
	bad.Connection.Generation = "other"
	_, err = admissions.AdmitTrackerStatusWrite(ctx, bad)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
	_, err = admissions.AdmitTrackerStatusWrite(ctx, p)
	require.ErrorIs(t, err, tracker.ErrUnavailable, "a selected binding alone is not live owner registry authority")
	_, err = store.(db.TrackerRegistryStore).ReconcileTrackerRegistry(ctx, epoch, []tracker.RegistryPolicy{{Selection: selection, Provider: "notion", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}})
	require.NoError(t, err)
	f, err := admissions.AdmitTrackerStatusWrite(ctx, p)
	require.NoError(t, err)
	require.Equal(t, db.TrackerWriteDispatching, f.Phase)
	_, err = admissions.AdmitTrackerStatusWrite(ctx, p)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown)
	// Definite result retention precedes root application; acknowledgement and
	// exact-event clearing must commit together in the native observation path.
	receipt := db.TrackerStatusResult{Status: "closed", Version: at, Locator: "22222222-2222-4222-8222-222222222222"}
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	confirmed := f
	confirmed.Phase = db.TrackerWriteConfirmed
	confirmed.ProviderAcknowledged = true
	confirmed.Result = raw
	fences := store.(db.TrackerFenceStore)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &f, confirmed, epoch))
	wrongLocator := "another-item"
	_, _, err = store.(db.IssueStatusWriter).ObserveIssueStatus(ctx, db.IssueStatusObservationParams{Guard: p.Guard, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: fixture.Issue.UID, Observation: db.IssueStatusObservation{Version: at}, Status: "closed", ServicedEventUID: p.PendingEventUID, TrackerWriteUID: f.WriteUID, RemoteLocator: &wrongLocator})
	require.ErrorIs(t, err, db.ErrTrackerFenceInvalid)
	_, _, err = store.(db.IssueStatusWriter).ObserveIssueStatus(ctx, db.IssueStatusObservationParams{Guard: p.Guard, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: fixture.Issue.UID, Observation: db.IssueStatusObservation{Version: at}, Status: "closed", ServicedEventUID: p.PendingEventUID, TrackerWriteUID: f.WriteUID})
	require.NoError(t, err)
	acknowledged, found, err := fences.LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, db.TrackerWriteAcknowledged, acknowledged.Phase)
	current, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, p.Guard, mapping.ID)
	require.NoError(t, err)
	require.Empty(t, current.State.PendingEventUID)
	_, reopened, changed, err := store.ReopenIssue(ctx, fixture.Issue.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	p.PendingEventUID = reopened.UID
	p.Target = "open"
	_, err = admissions.AdmitTrackerStatusWrite(ctx, p)
	require.NoError(t, err)
	later := at.Add(time.Hour)
	_, claimed, err = store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", later, later.Add(-30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	p.Guard.StartedAt = later
	p.Now = later
	_, err = admissions.AdmitTrackerStatusWrite(ctx, p)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "claim expiry is not a mutation retry grant")
	// A retained response can be acknowledged even after a newer local intent
	// and newer read observation replace the pending row. No resend or inward
	// status update is needed to retire that exact proven historical delivery.
	head, _, err := fences.LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	proven := head
	proven.Phase = db.TrackerWriteConfirmed
	proven.ProviderAcknowledged = true
	proven.Result, err = json.Marshal(db.TrackerStatusResult{Status: "open", Version: later})
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &head, proven, epoch))
	_, newer, changed, err := store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "member", "Newer intent", nil)
	require.NoError(t, err)
	require.True(t, changed)
	receiptStore := store.(db.TrackerStatusReceiptStore)
	require.NoError(t, receiptStore.AcknowledgeRetainedTrackerStatus(ctx, p.Guard))
	current, err = store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, p.Guard, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, newer[0].UID, current.State.PendingEventUID)
	local, err := store.IssueByID(ctx, fixture.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", local.Status)
	head, _, err = fences.LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.Equal(t, db.TrackerWriteAcknowledged, head.Phase)
	require.NoError(t, receiptStore.AcknowledgeRetainedTrackerStatus(ctx, p.Guard), "idempotent recovery")
	return nil
}
