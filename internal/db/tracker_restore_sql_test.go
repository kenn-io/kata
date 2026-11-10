package db_test

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Contract: restore compares same-sequence and adjacent evidence across heads,
// histories and archived attempts, including evidence retained by the target.
func TestTrackerRestoreIndexedEvidence(t *testing.T) {
	first := trackerFenceFixture()
	first.Phase = db.TrackerWriteNotSent
	first.Result = []byte(`{"not_sent":true}`)
	base, err := db.TrackerExecutionFenceKey(first.RootInstanceUID, first.ProjectUID, first.Role)
	require.NoError(t, err)
	second := first
	second.Sequence = 2
	second.PreviousHead = db.TrackerFenceHead(first)
	second.Phase = db.TrackerWritePrepared
	second.Result = nil
	record := func(key string, fence db.TrackerExecutionFence) *db.MetaKV {
		t.Helper()
		raw, err := json.Marshal(fence)
		require.NoError(t, err)
		return &db.MetaKV{Key: key, Value: string(raw)}
	}
	attempt := record(base+".attempt."+first.WriteUID+".00000000000000000001", first)
	head := record(base, second)
	badSecond := second
	badSecond.PreviousHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	conflict := record(base, badSecond)
	other := first
	other.Target = "open"
	other.WriteUID, err = db.TrackerStatusWriteUID(other.RootInstanceUID, other.ProjectUID, other.SourceKey, other.Locator, other.PendingEventUID, other.Target)
	require.NoError(t, err)
	settledHead := first
	settledHead.ProcessEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	otherLane := first
	otherLane.Role = "coordinator"
	otherBase, err := db.TrackerExecutionFenceKey(otherLane.RootInstanceUID, otherLane.ProjectUID, otherLane.Role)
	require.NoError(t, err)
	otherAttempt := record(base+".attempt."+other.WriteUID+".00000000000000000001", other)
	for _, tc := range []struct {
		name     string
		retained *db.MetaKV
		incoming []db.ImportRecord
		wantErr  error
	}{
		{"forward", nil, []db.ImportRecord{attempt, head}, nil},
		{"reverse", nil, []db.ImportRecord{head, attempt}, nil},
		{"retained predecessor", attempt, []db.ImportRecord{head}, nil},
		{"retained successor", head, []db.ImportRecord{attempt}, nil},
		{"conflicting predecessor", attempt, []db.ImportRecord{conflict}, db.ErrTrackerOutcomeUnknown},
		{"conflicting successor", conflict, []db.ImportRecord{attempt}, db.ErrTrackerOutcomeUnknown},
		{"conflicting sequence", attempt, []db.ImportRecord{otherAttempt}, db.ErrTrackerOutcomeUnknown},
		{"same key", attempt, []db.ImportRecord{attempt}, nil},
		{"same key conflict", head, []db.ImportRecord{conflict}, db.ErrTrackerOutcomeUnknown},
		{"settled local epoch refresh", attempt, []db.ImportRecord{record(base, settledHead)}, nil},
		{"independent lane", attempt, []db.ImportRecord{record(otherBase, otherLane)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			tx, err := store.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			if tc.retained != nil {
				_, err := tx.ExecContext(t.Context(), `INSERT INTO meta(key,value) VALUES(?,?)`, tc.retained.Key, tc.retained.Value)
				require.NoError(t, err)
			}
			err = db.ValidateTrackerMetadataRestoreSQL(t.Context(), tx, tc.incoming, db.ImportOptions{})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
