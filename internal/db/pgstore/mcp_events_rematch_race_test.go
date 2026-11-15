package pgstore_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

// rematchRaceStore opens a store holding three issue creations whose event
// ids a pull can record as applied.
func rematchRaceStore(t *testing.T) (*pgstore.Store, [3]db.Event) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "rematch-project")
	require.NoError(t, err)
	var events [3]db.Event
	for i := range events {
		_, events[i], err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "applied", Author: "tester"})
		require.NoError(t, err)
	}
	return store, events
}

// recordRematch records applied events in a transaction the caller commits.
func recordRematch(t *testing.T, store *pgstore.Store, events ...db.Event) *sql.Tx {
	t.Helper()
	tx, err := store.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	uids := make([]string, len(events))
	for i, event := range events {
		uids[i] = event.UID
	}
	require.NoError(t, db.RecordMCPEventsRematch(t.Context(), tx, uids))
	return tx
}

// waitForBlockedWriter waits until a statement in this database waits for a
// lock another transaction holds.
func waitForBlockedWriter(t *testing.T, store *pgstore.Store) {
	t.Helper()
	//nolint:kennlint // sleeptest: only the PostgreSQL server reports that another backend waits on a lock; bounded polling observes it.
	require.Eventually(t, func() bool {
		var waiting bool
		err := store.QueryRowContext(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "the second writer never waited for the first")
}

func rematchRange(t *testing.T, store *pgstore.Store) [2]int64 {
	t.Helper()
	from, through, _, ok, err := store.MCPEventsRematch(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	return [2]int64{from, through}
}

// A pull commits a wider range after a worker read the record but before its
// partial acknowledgment writes. The acknowledgment must not replace it.
func TestMCPEventsRematchAcknowledgmentKeepsAPullCommittedMeanwhile(t *testing.T) {
	store, events := rematchRaceStore(t)
	ctx := t.Context()
	require.NoError(t, recordRematch(t, store, events[0], events[1]).Commit())
	_, _, token, _, err := store.MCPEventsRematch(ctx)
	require.NoError(t, err)

	pull := recordRematch(t, store, events[2])
	acked := make(chan bool, 1)
	go func() {
		done, err := store.AcknowledgeMCPEventsRematch(context.WithoutCancel(ctx), token, events[0].ID)
		acked <- err == nil && done
	}()
	waitForBlockedWriter(t, store)
	require.NoError(t, pull.Commit())

	select {
	case done := <-acked:
		assert.False(t, done, "the record changed under the acknowledgment")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the acknowledgment stayed blocked after the pull committed")
	}
	assert.Equal(t, [2]int64{events[0].ID, events[2].ID}, rematchRange(t, store))
}

// Two pulls apply events at once. The second waits for the first and keeps
// both ranges, whether or not a record existed before either.
func TestMCPEventsRematchConcurrentPullsKeepBothRanges(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "no record", true: "existing record"}[existing], func(t *testing.T) {
			store, events := rematchRaceStore(t)
			if existing {
				require.NoError(t, recordRematch(t, store, events[1]).Commit())
			}
			first := recordRematch(t, store, events[0])
			second := make(chan error, 1)
			go func() {
				tx, err := store.BeginTx(context.WithoutCancel(t.Context()), nil)
				if err == nil {
					err = db.RecordMCPEventsRematch(context.WithoutCancel(t.Context()), tx, []string{events[2].UID})
					if err == nil {
						err = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
				}
				second <- err
			}()
			waitForBlockedWriter(t, store)
			require.NoError(t, first.Commit())

			select {
			case err := <-second:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "the second pull stayed blocked after the first committed")
			}
			assert.Equal(t, [2]int64{events[0].ID, events[2].ID}, rematchRange(t, store))
		})
	}
}
