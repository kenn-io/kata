package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

func checkTrackerControlReceipts(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-control-project")
	require.NoError(t, err)
	control := tracker.ControlReceipt{Version: 1, RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, UID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Principal: tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}, Selection: tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Kind: tracker.ControlBind, SourceKey: "notion:source", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: tracker.ControlPending}
	receipts := store.(db.TrackerControlStore)
	admitted, fresh, err := receipts.BeginTrackerControl(ctx, control)
	require.NoError(t, err)
	require.True(t, fresh)
	require.Equal(t, control, admitted)
	admitted, fresh, err = receipts.BeginTrackerControl(ctx, control)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, control, admitted)
	changed := control
	changed.RequestDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, _, err = receipts.BeginTrackerControl(ctx, changed)
	require.ErrorIs(t, err, tracker.ErrControlConflict)
	done := control
	done.State = tracker.ControlComplete
	done.Result = &tracker.ControlResult{Code: "ok", Enabled: true}
	require.NoError(t, receipts.CompleteTrackerControl(ctx, control, done))
	existing, fresh, err := receipts.BeginTrackerControl(ctx, control)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, done, existing)
	_, _, err = receipts.BeginTrackerControl(db.WithAuthorizedProjects(ctx, []string{}), control)
	require.Error(t, err)
	return nil
}

func checkTrackerControlCapacity(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-capacity-project")
	require.NoError(t, err)
	base := tracker.ControlReceipt{Version: 1, RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}, Selection: tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Kind: tracker.ControlRun, SourceKey: "notion:source", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: tracker.ControlPending}
	receipts := store.(db.TrackerControlStore)
	var first tracker.ControlReceipt
	for i := range 63 {
		p := base
		p.UID, err = uid.New()
		require.NoError(t, err)
		_, fresh, err := receipts.BeginTrackerControl(ctx, p)
		require.NoError(t, err)
		require.True(t, fresh)
		if i == 0 {
			first = p
		}
	}
	// Race for the final slot before exercising the compatibility rebuild.
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	candidates := make([]tracker.ControlReceipt, 2)
	for i := range errs {
		candidates[i] = base
		candidates[i].UID, err = uid.New()
		require.NoError(t, err)
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _, errs[i] = receipts.BeginTrackerControl(ctx, candidates[i]) }()
	}
	close(start)
	wg.Wait()
	var overflow tracker.ControlReceipt
	successes, full := 0, 0
	for i, err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, tracker.ErrControlCapacity) {
			full++
			overflow = candidates[i]
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, full)
	var counter db.MetaKV
	for record, exportErr := range store.ExportMeta(ctx) {
		require.NoError(t, exportErr)
		if strings.HasPrefix(record.Key, db.TrackerMetadataPrefix+"control_capacity.") {
			require.Empty(t, counter.Key)
			counter = record
		}
	}
	require.NotEmpty(t, counter.Key)
	_, keep, err := db.TrackerMetadataForImport(counter, db.ImportOptions{})
	require.NoError(t, err)
	require.False(t, keep)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = sqlStore.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, counter.Key)
	require.NoError(t, err)
	for record, exportErr := range store.ExportMeta(ctx) {
		require.NoError(t, exportErr)
		require.NotEqual(t, counter.Key, record.Key)
	}
	_, _, err = receipts.BeginTrackerControl(ctx, overflow)
	require.ErrorIs(t, err, tracker.ErrControlCapacity)
	done := first
	done.State = tracker.ControlComplete
	done.Result = &tracker.ControlResult{Code: "ok"}
	require.NoError(t, receipts.CompleteTrackerControl(ctx, first, done))
	_, fresh, err := receipts.BeginTrackerControl(ctx, overflow)
	require.NoError(t, err)
	require.True(t, fresh)
	retained, fresh, err := receipts.BeginTrackerControl(ctx, first)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, done, retained, "capacity must not evict replay evidence")
	return nil
}

// A restore retires pending effects and resets their local capacity counts.
func checkTrackerControlRestoreCapacity(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-restore-project")
	require.NoError(t, err)
	base := tracker.ControlReceipt{Version: 1, RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}, Selection: tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Kind: tracker.ControlRun, SourceKey: "notion:source", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: tracker.ControlPending}
	receipts := store.(db.TrackerControlStore)
	base.UID, err = uid.New()
	require.NoError(t, err)
	_, _, err = receipts.BeginTrackerControl(ctx, base)
	require.NoError(t, err)
	sqlStore := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	tx, err := sqlStore.BeginTx(ctx, nil)
	require.NoError(t, err)
	retained, err := db.RetainTrackerMetadataSQL(ctx, tx)
	require.NoError(t, err)
	require.NoError(t, db.RestoreRetainedTrackerMetadataSQL(ctx, tx, retained))
	var counter string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, db.TrackerControlCapacityKey).Scan(&counter))
	require.JSONEq(t, `{"pending":0,"projects":{},"connections":{}}`, counter)
	require.NoError(t, tx.Commit())
	for range 64 {
		p := base
		p.UID, err = uid.New()
		require.NoError(t, err)
		_, fresh, err := receipts.BeginTrackerControl(ctx, p)
		require.NoError(t, err)
		require.True(t, fresh)
	}
	return nil
}
