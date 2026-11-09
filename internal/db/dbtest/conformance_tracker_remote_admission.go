package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

// Contract: a remote preparation reserves but cannot send. Final admission
// checks the original event/claim and exact owner predecessor again; root ack
// and owner checkpoint advancement share the observation transaction.
func checkTrackerRemoteStatusAdmission(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "remote-admission-project", "Mapped task", "member", nil)
	require.NoError(t, err)
	ownerStore := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, ownerStore.Close()) })
	projectRecords, err := CollectImportRecords(ctx, store, db.ExportFilter{ProjectID: &fixture.Project.ID})
	require.NoError(t, err)
	require.NoError(t, ownerStore.ImportReplay(ctx, projectRecords, db.ImportOptions{PreserveIssueSyncBindingEnabled: true, NewInstance: true}))
	require.NotEqual(t, store.InstanceUID(), ownerStore.InstanceUID())
	selection := db.IssueSyncConnection{OwnerInstanceUID: ownerStore.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first"}
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "example-item", ObjectType: "issue", IssueID: &fixture.Issue.ID})
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
	_, err = admissions.AdmitTrackerStatusWrite(ctx, p)
	require.Error(t, err, "a remote selection requires explicit owner-lane reservation")
	p.RemoteAttempt = &db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", ArgumentsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", OwnerSequence: 1, RootEpoch: epoch, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2"}
	prepared, err := admissions.AdmitTrackerStatusWrite(ctx, p)
	require.NoError(t, err)
	require.Equal(t, db.TrackerWritePrepared, prepared.Phase)
	_, err = ownerStore.(db.TrackerRegistryStore).ReconcileTrackerRegistry(ctx, epoch, []tracker.RegistryPolicy{{Selection: selection, Provider: "notion", Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}})
	require.NoError(t, err)
	ownerExecutor := ownerStore.(db.TrackerExecutorStore)
	ownerPrepared, err := ownerExecutor.PrepareTrackerExecutor(ctx, prepared, "notion", epoch)
	require.NoError(t, err)
	hypothetical := ownerPrepared
	hypothetical.Phase = db.TrackerWriteConfirmed
	hypothetical.ProviderAcknowledged = true
	hypothetical.Result, err = json.Marshal(db.TrackerStatusResult{Status: "closed", Version: at})
	require.NoError(t, err)
	require.ErrorIs(t, store.(db.TrackerRemoteRecoveryStore).AcceptTrackerOwnerResult(ctx, hypothetical), db.ErrTrackerOutcomeUnknown, "prepared root issued no permit")
	p.ExpectedReservation = &prepared
	bad := p
	changedPreparation := prepared
	changedRemote := *prepared.Remote
	changedRemote.OwnerEpoch = epoch
	changedPreparation.Remote = &changedRemote
	bad.ExpectedReservation = &changedPreparation
	_, err = admissions.AdmitTrackerStatusWrite(ctx, bad)
	require.Error(t, err)
	dispatched, err := admissions.AdmitTrackerStatusWrite(ctx, p)
	require.NoError(t, err)
	require.Equal(t, db.TrackerWriteDispatching, dispatched.Phase)
	// Even exact-looking owner non-send evidence must not relabel a root
	// dispatching/possibly sent attempt as eligible for retry.
	unconfirmedNonSend := ownerPrepared
	unconfirmedNonSend.Phase = db.TrackerWriteNotSent
	unconfirmedNonSend.Result = []byte(`{"not_sent":true}`)
	require.ErrorIs(t, store.(db.TrackerRemoteRecoveryStore).SettleTrackerOwnerNonSend(ctx, unconfirmedNonSend), db.ErrTrackerOutcomeUnknown)
	sqlStore, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	headKey, err := db.TrackerExecutionFenceKey(dispatched.RootInstanceUID, dispatched.ProjectUID, dispatched.Role)
	require.NoError(t, err)
	unknownHead := dispatched
	unknownHead.Phase = db.TrackerWriteUnknown
	unknownRaw, err := json.Marshal(unknownHead)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(unknownRaw), headKey)
	require.NoError(t, err)
	require.ErrorIs(t, store.(db.TrackerRemoteRecoveryStore).SettleTrackerOwnerNonSend(ctx, unconfirmedNonSend), db.ErrTrackerOutcomeUnknown, "an unknown root remains unknown even with a claimed non-send")
	dispatchedRaw, err := json.Marshal(dispatched)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(dispatchedRaw), headKey)
	require.NoError(t, err)
	_, err = admissions.AdmitTrackerStatusWrite(ctx, p)
	require.Error(t, err, "a duplicate permit cannot create a second dispatch")
	ownerDispatched, err := ownerExecutor.AdmitTrackerExecutor(ctx, ownerPrepared, "notion", epoch)
	require.NoError(t, err)
	ownerConfirmed := ownerDispatched
	ownerConfirmed.Phase = db.TrackerWriteConfirmed
	ownerConfirmed.ProviderAcknowledged = true
	ownerConfirmed.Result, err = json.Marshal(db.TrackerStatusResult{Status: "closed", Version: at})
	require.NoError(t, err)
	require.NoError(t, ownerStore.(db.TrackerFenceStore).StoreTrackerExecutionFence(ctx, &ownerDispatched, ownerConfirmed, epoch))
	recovery := store.(db.TrackerRemoteRecoveryStore)
	require.NoError(t, recovery.AcceptTrackerOwnerResult(ctx, ownerConfirmed))
	require.NoError(t, recovery.AcceptTrackerOwnerResult(ctx, ownerConfirmed), "exact original receipt replay")
	conflict := ownerConfirmed
	conflict.Result, err = json.Marshal(db.TrackerStatusResult{Status: "closed", Version: at.Add(time.Second)})
	require.NoError(t, err)
	require.ErrorIs(t, recovery.AcceptTrackerOwnerResult(ctx, conflict), db.ErrTrackerFenceChanged)
	conflict = ownerConfirmed
	attemptCopy := *conflict.Remote
	attemptCopy.UID = "01ARZ3NDEKTSV4RRFFQ69G5FB9"
	conflict.Remote = &attemptCopy
	require.ErrorIs(t, recovery.AcceptTrackerOwnerResult(ctx, conflict), db.ErrTrackerFenceChanged)
	_, _, err = store.(db.IssueStatusWriter).ObserveIssueStatus(ctx, db.IssueStatusObservationParams{Guard: p.Guard, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: fixture.Issue.UID, Observation: db.IssueStatusObservation{Version: at}, Status: "closed", ServicedEventUID: p.PendingEventUID, TrackerWriteUID: dispatched.WriteUID})
	require.NoError(t, err)
	checkpoint, found, err := store.(db.TrackerOwnerCheckpointStore).LoadTrackerOwnerCheckpoint(ctx, store.InstanceUID(), fixture.Project.UID, selection.OwnerInstanceUID)
	require.NoError(t, err)
	require.True(t, found)
	owner, err := db.TrackerExecutorPreparation(prepared, epoch)
	require.NoError(t, err)
	require.Equal(t, db.TrackerFenceHead(owner), checkpoint.Head)
	require.Equal(t, db.TrackerWriteAcknowledged, checkpoint.Disposition)
	require.NoError(t, ownerExecutor.AcknowledgeTrackerExecutor(ctx, checkpoint))
	_, err = store.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, "01ARZ3NDEKTSV4RRFFQ69G5FB3")
	require.NoError(t, err)
	retained, present, err := store.(db.TrackerFenceStore).LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, present)
	require.NoError(t, retained.Validate())
	require.Equal(t, epoch, retained.Remote.RootEpoch, "activation does not rewrite the old remote reservation")

	// A root can retire a preparation before the owner gets its admission
	// response. Later exact non-send evidence advances only that owner's lane,
	// even when another owner now occupies the global unknown barrier.
	_, reopened, changed, err := store.ReopenIssue(ctx, fixture.Issue.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	later := at.Add(time.Hour)
	_, claimed, err = store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", later, later.Add(-30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	p.Guard.StartedAt = later
	p.Now = later
	p.ProcessEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB3"
	p.Target = "open"
	p.PendingEventUID = reopened.UID
	p.ExpectedReservation = nil
	p.RemoteAttempt = &db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB4", ArgumentsDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", OwnerSequence: 2, OwnerPreviousHead: checkpoint.Head, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2"}
	reservation, err := admissions.AdmitTrackerStatusWrite(ctx, p)
	require.NoError(t, err)
	retired := reservation
	retired.Phase = db.TrackerWriteNotSent
	retired.Result = []byte(`{"not_sent":true}`)
	fences := store.(db.TrackerFenceStore)
	retired, err = store.(db.TrackerCoordinatorRetirementStore).RetireTrackerCoordinatorPreparation(ctx, reservation, p.ProcessEpoch)
	require.NoError(t, err)
	require.Equal(t, db.TrackerWriteNotSent, retired.Phase)
	checkpoint, _, err = store.(db.TrackerOwnerCheckpointStore).LoadTrackerOwnerCheckpoint(ctx, store.InstanceUID(), fixture.Project.UID, selection.OwnerInstanceUID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), checkpoint.Sequence, "root-only retirement is not owner acknowledgement")
	unrelated := retired
	unrelated.Sequence++
	unrelated.PreviousHead = db.TrackerFenceHead(retired)
	unrelated.Connection.OwnerInstanceUID = "01ARZ3NDEKTSV4RRFFQ69G5FB5"
	unrelated.Phase = db.TrackerWritePrepared
	unrelated.Result = nil
	unrelated.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB6"
	unrelated.WriteUID, err = db.TrackerStatusWriteUID(unrelated.RootInstanceUID, unrelated.ProjectUID, unrelated.SourceKey, unrelated.Locator, unrelated.PendingEventUID, unrelated.Target)
	require.NoError(t, err)
	unrelated.Remote = &db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB7", ArgumentsDigest: p.RemoteAttempt.ArgumentsDigest, RootSequence: unrelated.Sequence, RootPreviousHead: unrelated.PreviousHead, OwnerSequence: 1, RootEpoch: p.ProcessEpoch, OwnerEpoch: p.RemoteAttempt.OwnerEpoch}
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &retired, unrelated, p.ProcessEpoch))
	unknown := unrelated
	unknown.Phase = db.TrackerWriteUnknown
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &unrelated, unknown, p.ProcessEpoch))
	ownerPreparation, err := ownerExecutor.PrepareTrackerExecutor(ctx, reservation, "notion", epoch)
	require.NoError(t, err)
	ownerRecovery := ownerStore.(db.TrackerRemoteRecoveryStore)
	proof, err := ownerRecovery.RetireTrackerExecutorPreparation(ctx, ownerPreparation, "01ARZ3NDEKTSV4RRFFQ69G5FC0")
	require.NoError(t, err, "a new locked process may retire a proven prepared attempt, never a dispatched one")
	_, err = ownerRecovery.RetireTrackerExecutorPreparation(ctx, ownerDispatched, epoch)
	require.Error(t, err, "exact prepared CAS cannot retire a dispatched or superseded attempt")
	restoredProof := proof
	restoredProof.ActivationRequired = true
	require.Error(t, recovery.SettleTrackerOwnerNonSend(ctx, restoredProof), "restored preparation is no proof of non-send")
	require.NoError(t, recovery.SettleTrackerOwnerNonSend(ctx, proof))
	require.NoError(t, recovery.SettleTrackerOwnerNonSend(ctx, proof), "exact replay")
	checkpoint, _, err = store.(db.TrackerOwnerCheckpointStore).LoadTrackerOwnerCheckpoint(ctx, store.InstanceUID(), fixture.Project.UID, selection.OwnerInstanceUID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), checkpoint.Sequence)
	require.Equal(t, db.TrackerWriteNotSent, checkpoint.Disposition)
	current, _, err := fences.LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.Equal(t, unknown, current, "another owner's unknown write remains blocked")
	_, err = store.(db.TrackerCoordinatorRetirementStore).RetireTrackerCoordinatorPreparation(ctx, unknown, p.ProcessEpoch)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "operator authority cannot retire an unknown dispatch as non-send")
	pending, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, p.Guard, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, reopened.UID, pending.State.PendingEventUID, "non-send settlement cannot consume intent")
	return nil
}

// A real owner retirement after a root permit cannot clear root uncertainty.
func checkTrackerNonSendAfterPermit(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "tracker-permit-project", "Mapped task", "member", nil)
	require.NoError(t, err)
	owner := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	records, err := CollectImportRecords(ctx, store, db.ExportFilter{ProjectID: &fixture.Project.ID})
	require.NoError(t, err)
	require.NoError(t, owner.ImportReplay(ctx, records, db.ImportOptions{PreserveIssueSyncBindingEnabled: true, NewInstance: true}))
	selection := db.IssueSyncConnection{OwnerInstanceUID: owner.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Generation: "first"}
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "example-item", ObjectType: "issue", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	epoch := "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	_, err = store.(db.TrackerActivationStore).ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, epoch)
	require.NoError(t, err)
	_, events, changed, err := store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "member", "Completed task", nil)
	require.NoError(t, err)
	require.True(t, changed)
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	request := db.TrackerStatusAdmission{Guard: db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: at, BindingUpdatedAt: &binding.UpdatedAt}, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: fixture.Issue.UID, PendingEventUID: events[0].UID, Target: "closed", Connection: selection, ProcessEpoch: epoch, Now: at, RemoteAttempt: &db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", ArgumentsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", OwnerSequence: 1, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2"}}
	admissions := store.(db.TrackerStatusStore)
	// Seed an unrelated settled root sequence so coordinator and owner lane
	// sequence numbers differ throughout the retry.
	seed := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: store.InstanceUID(), ProjectUID: fixture.Project.UID, Connection: db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "seed"}, Sequence: 1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", SourceKey: "synthetic:source", Locator: "seed-item", Target: "open", ProcessEpoch: epoch, Phase: db.TrackerWritePrepared}
	seed.WriteUID, err = db.TrackerStatusWriteUID(seed.RootInstanceUID, seed.ProjectUID, seed.SourceKey, seed.Locator, seed.PendingEventUID, seed.Target)
	require.NoError(t, err)
	require.NoError(t, store.(db.TrackerFenceStore).StoreTrackerExecutionFence(ctx, nil, seed, epoch))
	seedRetired := seed
	seedRetired.Phase = db.TrackerWriteNotSent
	seedRetired.Result = []byte(`{"not_sent":true}`)
	require.NoError(t, store.(db.TrackerFenceStore).StoreTrackerExecutionFence(ctx, &seed, seedRetired, epoch))
	reservation, err := admissions.AdmitTrackerStatusWrite(ctx, request)
	require.NoError(t, err)
	require.NotEqual(t, reservation.Sequence, reservation.Remote.OwnerSequence)
	ownerEpoch := "01ARZ3NDEKTSV4RRFFQ69G5FB8"
	_, err = owner.(db.TrackerRegistryStore).ReconcileTrackerRegistry(ctx, ownerEpoch, []tracker.RegistryPolicy{{Selection: selection, Provider: "notion", Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}})
	require.NoError(t, err)
	ownerPrepared, err := owner.(db.TrackerExecutorStore).PrepareTrackerExecutor(ctx, reservation, "notion", ownerEpoch)
	require.NoError(t, err)
	firstProof, err := owner.(db.TrackerRemoteRecoveryStore).RetireTrackerExecutorPreparation(ctx, ownerPrepared, ownerEpoch)
	require.NoError(t, err)
	require.NoError(t, store.(db.TrackerRemoteRecoveryStore).SettleTrackerOwnerNonSend(ctx, firstProof))
	priorRoot, found, err := store.(db.TrackerFenceStore).LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	retry := reservation
	retry.Sequence++
	retry.PreviousHead = db.TrackerFenceHead(priorRoot)
	remote := *reservation.Remote
	remote.UID = "01ARZ3NDEKTSV4RRFFQ69G5FB3"
	remote.OwnerEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB6"
	remote.RootSequence = retry.Sequence
	remote.RootPreviousHead = retry.PreviousHead
	remote.OwnerSequence++
	remote.OwnerPreviousHead = db.TrackerFenceHead(firstProof)
	retry.Remote = &remote
	request.RemoteAttempt = nil
	proposal := db.TrackerRemoteRetryPreparation{PriorOwner: firstProof, Reservation: retry}
	policy := []tracker.RegistryPolicy{{Selection: selection, Provider: "notion", Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}
	restartedEpoch := "01ARZ3NDEKTSV4RRFFQ69G5FB9"
	_, err = owner.(db.TrackerRegistryStore).ReconcileTrackerRegistry(ctx, restartedEpoch, policy)
	require.NoError(t, err)
	_, err = owner.(db.TrackerExecutorRetryStore).PrepareTrackerExecutorRetry(ctx, proposal, "notion", restartedEpoch)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "a later owner process cannot reuse prior process evidence")
	_, err = owner.(db.TrackerExecutorRetryStore).PrepareTrackerExecutorRetry(ctx, proposal, "notion", ownerEpoch)
	require.Error(t, err, "old process evidence cannot bypass a registry reconciled for the new process")
	// Restore the original synthetic registry epoch to exercise the separate
	// success path with the same retained owner process evidence.
	_, err = owner.(db.TrackerRegistryStore).ReconcileTrackerRegistry(ctx, ownerEpoch, policy)
	require.NoError(t, err)
	wrong := proposal
	wrong.PriorOwner.ProcessEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB4"
	_, err = owner.(db.TrackerExecutorRetryStore).PrepareTrackerExecutorRetry(ctx, wrong, "notion", ownerEpoch)
	require.Error(t, err, "the owner must match exact retained process evidence")
	require.ErrorIs(t, store.(db.TrackerFenceStore).StoreTrackerExecutionFence(ctx, &priorRoot, retry, epoch), db.ErrTrackerOutcomeUnknown, "generic store cannot authorize remote retry")
	checkpoint, found, err := store.(db.TrackerOwnerCheckpointStore).LoadTrackerOwnerCheckpoint(ctx, store.InstanceUID(), fixture.Project.UID, selection.OwnerInstanceUID)
	require.NoError(t, err)
	require.True(t, found)
	sqlStore, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	checkpointKey := db.TrackerOwnerCheckpointKey(store.InstanceUID(), fixture.Project.UID, selection.OwnerInstanceUID)
	wrongCheckpoint := checkpoint
	wrongCheckpoint.ResultDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	wrongCheckpointRaw, err := json.Marshal(wrongCheckpoint)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(wrongCheckpointRaw), checkpointKey)
	require.NoError(t, err)
	_, err = store.(db.TrackerStatusRetryStore).PrepareTrackerStatusRetry(ctx, request, proposal)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "checkpoint must match the full owner proof")
	checkpointRaw, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(checkpointRaw), checkpointKey)
	require.NoError(t, err)
	rootKey, err := db.TrackerExecutionFenceKey(priorRoot.RootInstanceUID, priorRoot.ProjectUID, priorRoot.Role)
	require.NoError(t, err)
	archiveKey := fmt.Sprintf("%s.attempt.%s.%020d", rootKey, priorRoot.WriteUID, priorRoot.Sequence)
	_, err = sqlStore.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, archiveKey, `{}`)
	require.NoError(t, err)
	_, err = store.(db.TrackerStatusRetryStore).PrepareTrackerStatusRetry(ctx, request, proposal)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "a conflicting archive cannot be overwritten")
	stillRetained, found, err := store.(db.TrackerFenceHistoryStore).LoadTrackerExecutionHistory(ctx, priorRoot.RootInstanceUID, priorRoot.ProjectUID, priorRoot.Role, priorRoot.WriteUID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, priorRoot, stillRetained, "archive conflict preserves semantic history")
	_, err = sqlStore.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, archiveKey)
	require.NoError(t, err)
	statusMapping, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, request.Guard, mapping.ID)
	require.NoError(t, err)
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok)
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	native := db.TrackerFenceSQL{InstanceUID: store.InstanceUID()}
	_, err = native.PrepareStatusRetryInTransaction(ctx, tx, binding, statusMapping, request, proposal)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	stillRetained, found, err = store.(db.TrackerFenceHistoryStore).LoadTrackerExecutionHistory(ctx, priorRoot.RootInstanceUID, priorRoot.ProjectUID, priorRoot.Role, priorRoot.WriteUID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, priorRoot, stillRetained)
	_, found, err = store.(db.TrackerAttemptArchiveStore).LoadTrackerExecutionAttempt(ctx, priorRoot.RootInstanceUID, priorRoot.ProjectUID, priorRoot.Role, priorRoot.WriteUID, priorRoot.Sequence)
	require.NoError(t, err)
	require.False(t, found, "rollback removes an inserted archive")
	rolledBackHead, found, err := store.(db.TrackerFenceStore).LoadTrackerExecutionFence(ctx, priorRoot.RootInstanceUID, priorRoot.ProjectUID, priorRoot.Role)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, priorRoot, rolledBackHead, "rollback preserves the prior head")
	rootRetry, err := store.(db.TrackerStatusRetryStore).PrepareTrackerStatusRetry(ctx, request, proposal)
	require.NoError(t, err)
	require.Equal(t, retry, rootRetry)
	_, err = store.(db.TrackerStatusRetryStore).PrepareTrackerStatusRetry(ctx, request, proposal)
	require.Error(t, err, "a witnessed reservation cannot advance root twice")
	ownerRetry, err := owner.(db.TrackerExecutorRetryStore).PrepareTrackerExecutorRetry(ctx, proposal, "notion", ownerEpoch)
	require.NoError(t, err)
	_, err = owner.(db.TrackerExecutorRetryStore).PrepareTrackerExecutorRetry(ctx, proposal, "notion", ownerEpoch)
	require.Error(t, err, "a witnessed reservation cannot advance owner twice")
	retainedRoot, found, err := store.(db.TrackerFenceStore).LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, db.TrackerWritePrepared, retainedRoot.Phase, "retry preparation alone cannot dispatch")
	request.RemoteAttempt = retry.Remote
	request.ExpectedReservation = &retry
	dispatched, err := admissions.AdmitTrackerStatusWrite(ctx, request)
	require.NoError(t, err)
	proof, err := owner.(db.TrackerRemoteRecoveryStore).RetireTrackerExecutorPreparation(ctx, ownerRetry, ownerEpoch)
	require.NoError(t, err)
	require.ErrorIs(t, store.(db.TrackerRemoteRecoveryStore).SettleTrackerOwnerNonSend(ctx, proof), db.ErrTrackerOutcomeUnknown)
	retained, found, err := store.(db.TrackerFenceStore).LoadTrackerExecutionFence(ctx, store.InstanceUID(), fixture.Project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, dispatched, retained)
	return nil
}
