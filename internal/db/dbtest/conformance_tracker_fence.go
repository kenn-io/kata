package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Both native backends must retain the same conditional execution head in their
// existing meta store. This is a behavioral fixture, not a DDL-content assertion.
func checkTrackerExecutionFence(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	fences, ok := store.(db.TrackerFenceStore)
	require.True(t, ok)
	fence := db.TrackerExecutionFence{
		Version: 1, Role: "coordinator", RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID,
		Connection: db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"},
		Sequence:   1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", SourceKey: "notion:example-source", Locator: "example-item", Target: "closed",
		ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWritePrepared,
	}
	fence.WriteUID, err = db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator, fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, nil, fence, fence.ProcessEpoch))
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, nil, fence, fence.ProcessEpoch), db.ErrTrackerFenceChanged)
	loaded, found, err := fences.LoadTrackerExecutionFence(ctx, fence.RootInstanceUID, project.UID, fence.Role)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, fence, loaded)
	dispatching := fence
	dispatching.Phase = db.TrackerWriteDispatching
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &fence, dispatching, "01ARZ3NDEKTSV4RRFFQ69G5FB1"), db.ErrTrackerOutcomeUnknown)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &fence, dispatching, fence.ProcessEpoch))
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &fence, dispatching, fence.ProcessEpoch), db.ErrTrackerFenceChanged)
	unknown := dispatching
	unknown.Phase = db.TrackerWriteUnknown
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &dispatching, unknown, fence.ProcessEpoch))
	confirmed := unknown
	confirmed.Phase = db.TrackerWriteConfirmed
	confirmed.ProviderAcknowledged = true
	confirmed.Result = jsontext.Value(`{"status":"closed"}`)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &unknown, confirmed, fence.ProcessEpoch))
	acknowledged := confirmed
	acknowledged.Phase = db.TrackerWriteAcknowledged
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &confirmed, acknowledged, fence.ProcessEpoch))
	next := fence
	next.Sequence = 2
	next.PreviousHead = db.TrackerFenceHead(acknowledged)
	next.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	next.WriteUID, err = db.TrackerStatusWriteUID(next.RootInstanceUID, next.ProjectUID, next.SourceKey, next.Locator, next.PendingEventUID, next.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &acknowledged, next, next.ProcessEpoch))
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &acknowledged, next, next.ProcessEpoch), db.ErrTrackerFenceChanged, "an older root head cannot overwrite newer state")
	notSent := next
	notSent.Phase = db.TrackerWriteNotSent
	notSent.Result = jsontext.Value(`{"not_sent":true}`)
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &next, notSent, "01ARZ3NDEKTSV4RRFFQ69G5FB2"), db.ErrTrackerOutcomeUnknown)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &next, notSent, next.ProcessEpoch))
	// A proven non-send may retry the same intent only in the process that
	// retired it, with the same connection and its retained evidence intact.
	retry := next
	retry.Sequence++
	retry.PreviousHead = db.TrackerFenceHead(notSent)
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &notSent, retry, "01ARZ3NDEKTSV4RRFFQ69G5FB2"), db.ErrTrackerOutcomeUnknown)
	otherConnection := retry
	otherConnection.Connection.Generation = "other"
	require.Error(t, fences.StoreTrackerExecutionFence(ctx, &notSent, otherConnection, retry.ProcessEpoch))
	base, err := db.TrackerExecutionFenceKey(fence.RootInstanceUID, project.UID, fence.Role)
	require.NoError(t, err)
	wantArchive := fmt.Sprintf("%s.attempt.%s.%020d", base, notSent.WriteUID, notSent.Sequence)
	sqlStore, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	_, err = sqlStore.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, wantArchive, `{}`)
	require.NoError(t, err)
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &notSent, retry, retry.ProcessEpoch), db.ErrTrackerOutcomeUnknown)
	_, err = sqlStore.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, wantArchive)
	require.NoError(t, err)
	// A stale CAS after archive insertion must roll back both archive and
	// semantic-history deletion so the real current head remains authoritative.
	otherHead := notSent
	otherHead.ProcessEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB2"
	otherRaw, err := json.Marshal(otherHead)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(otherRaw), base)
	require.NoError(t, err)
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &notSent, retry, retry.ProcessEpoch), db.ErrTrackerFenceChanged)
	originalRaw, err := json.Marshal(notSent)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(originalRaw), base)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &notSent, retry, retry.ProcessEpoch))
	archived := false
	for record, exportErr := range store.ExportMeta(ctx) {
		require.NoError(t, exportErr)
		if record.Key == wantArchive {
			archived = true
			restored, keep, importErr := db.TrackerMetadataForImport(record, db.ImportOptions{})
			require.NoError(t, importErr)
			require.True(t, keep)
			require.NoError(t, db.CheckTrackerMetadataRestoreConflict(record.Value, restored))
		}
	}
	require.True(t, archived, "full backup retains the retired attempt")
	archiveStore := store.(db.TrackerAttemptArchiveStore)
	proof, found, err := archiveStore.LoadTrackerExecutionAttempt(ctx, fence.RootInstanceUID, project.UID, fence.Role, notSent.WriteUID, notSent.Sequence)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, notSent, proof)
	_, found, err = archiveStore.LoadTrackerExecutionAttempt(ctx, fence.RootInstanceUID, project.UID, fence.Role, notSent.WriteUID, notSent.Sequence+1)
	require.NoError(t, err)
	require.False(t, found)
	_, _, err = archiveStore.LoadTrackerExecutionAttempt(db.WithAuthorizedProjects(ctx, []string{}), fence.RootInstanceUID, project.UID, fence.Role, notSent.WriteUID, notSent.Sequence)
	require.Error(t, err)
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok)
	conflict := notSent
	conflict.Locator = "different-item"
	conflict.WriteUID, err = db.TrackerStatusWriteUID(conflict.RootInstanceUID, conflict.ProjectUID, conflict.SourceKey, conflict.Locator, conflict.PendingEventUID, conflict.Target)
	require.NoError(t, err)
	conflictRaw, err := json.Marshal(conflict)
	require.NoError(t, err)
	conflictRecord, keep, err := db.TrackerMetadataForImport(db.MetaKV{Key: base + ".history." + conflict.WriteUID, Value: string(conflictRaw)}, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.ErrorIs(t, db.CheckTrackerMetadataRestoreSQL(ctx, tx, conflictRecord, backend.Name == "postgres"), db.ErrTrackerOutcomeUnknown, "conflicting cross-key evidence cannot be combined")
	require.NoError(t, tx.Rollback())
	require.Error(t, fences.StoreTrackerExecutionFence(ctx, &notSent, retry, retry.ProcessEpoch), "retirement cannot authorize two successors")
	readmitted := retry
	readmitted.Phase = db.TrackerWriteDispatching
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &retry, readmitted, retry.ProcessEpoch))
	uncertain := readmitted
	uncertain.Phase = db.TrackerWriteUnknown
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &readmitted, uncertain, retry.ProcessEpoch))
	again := retry
	again.Sequence++
	again.PreviousHead = db.TrackerFenceHead(uncertain)
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &uncertain, again, again.ProcessEpoch), db.ErrTrackerOutcomeUnknown)
	_, err = sqlStore.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, base)
	require.NoError(t, err)
	_, _, err = fences.LoadTrackerExecutionFence(ctx, fence.RootInstanceUID, project.UID, fence.Role)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "inspection cannot report a missing head as safe when archives exist")
	genesis := next
	genesis.Sequence = 1
	genesis.PreviousHead = ""
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, nil, genesis, genesis.ProcessEpoch), db.ErrTrackerOutcomeUnknown, "a missing head does not erase archived semantic evidence")
	_, err = sqlStore.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, wantArchive)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, base+".attempt", `{}`)
	require.NoError(t, err)
	_, _, err = fences.LoadTrackerExecutionFence(ctx, fence.RootInstanceUID, project.UID, fence.Role)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "malformed bare attempt evidence also blocks inspection")
	denied := db.WithAuthorizedProjects(ctx, []string{})
	_, _, err = fences.LoadTrackerExecutionFence(denied, fence.RootInstanceUID, project.UID, fence.Role)
	require.Error(t, err)
	return nil
}

// An arbitrary not-sent payload is not the exact native non-send proof.
func checkTrackerNonSendProof(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-proof-project")
	require.NoError(t, err)
	fence := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Connection: db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Sequence: 1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", SourceKey: "notion:source", Locator: "item", Target: "closed", ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWritePrepared}
	fence.WriteUID, err = db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator, fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	storeFence := store.(db.TrackerFenceStore)
	require.NoError(t, storeFence.StoreTrackerExecutionFence(ctx, nil, fence, fence.ProcessEpoch))
	retired := fence
	retired.Phase = db.TrackerWriteNotSent
	retired.Result = jsontext.Value(`{"other":true}`)
	require.NoError(t, storeFence.StoreTrackerExecutionFence(ctx, &fence, retired, fence.ProcessEpoch))
	retry := fence
	retry.Sequence++
	retry.PreviousHead = db.TrackerFenceHead(retired)
	require.ErrorIs(t, storeFence.StoreTrackerExecutionFence(ctx, &retired, retry, fence.ProcessEpoch), db.ErrTrackerOutcomeUnknown)
	got, found, err := storeFence.LoadTrackerExecutionFence(ctx, fence.RootInstanceUID, project.UID, fence.Role)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, retired, got)
	history, found, err := store.(db.TrackerFenceHistoryStore).LoadTrackerExecutionHistory(ctx, fence.RootInstanceUID, project.UID, fence.Role, fence.WriteUID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, retired, history)
	_, found, err = store.(db.TrackerAttemptArchiveStore).LoadTrackerExecutionAttempt(ctx, fence.RootInstanceUID, project.UID, fence.Role, fence.WriteUID, fence.Sequence)
	require.NoError(t, err)
	require.False(t, found)
	return nil
}

// Contract: an acknowledged semantic event cannot be admitted again after an
// intervening event, even with a fresh sequence and connection generation.
func checkTrackerExecutionHistory(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-history-project")
	require.NoError(t, err)
	fences := store.(db.TrackerFenceStore)
	first := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Connection: db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Sequence: 1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", SourceKey: "notion:source", Locator: "item", Target: "closed", ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWritePrepared}
	first.WriteUID, err = db.TrackerStatusWriteUID(first.RootInstanceUID, first.ProjectUID, first.SourceKey, first.Locator, first.PendingEventUID, first.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, nil, first, first.ProcessEpoch))
	complete := func(f db.TrackerExecutionFence) db.TrackerExecutionFence {
		dispatch := f
		dispatch.Phase = db.TrackerWriteDispatching
		require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &f, dispatch, f.ProcessEpoch))
		confirmed := dispatch
		confirmed.Phase = db.TrackerWriteConfirmed
		confirmed.ProviderAcknowledged = true
		confirmed.Result = jsontext.Value(`{"status":"closed"}`)
		require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &dispatch, confirmed, f.ProcessEpoch))
		ack := confirmed
		ack.Phase = db.TrackerWriteAcknowledged
		require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &confirmed, ack, f.ProcessEpoch))
		return ack
	}
	ack1 := complete(first)
	second := first
	second.Sequence = 2
	second.PreviousHead = db.TrackerFenceHead(ack1)
	second.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	second.WriteUID, err = db.TrackerStatusWriteUID(second.RootInstanceUID, second.ProjectUID, second.SourceKey, second.Locator, second.PendingEventUID, second.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &ack1, second, second.ProcessEpoch))
	ack2 := complete(second)
	replay := first
	replay.Sequence = 3
	replay.PreviousHead = db.TrackerFenceHead(ack2)
	replay.Connection.Generation = "another"
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &ack2, replay, replay.ProcessEpoch), db.ErrTrackerOutcomeUnknown)
	return nil
}
