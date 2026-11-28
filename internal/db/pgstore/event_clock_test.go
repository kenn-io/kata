package pgstore

import (
	"cmp"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func openEventClockStore(t *testing.T, schema string) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	// A table this small is scanned sequentially, and a sequential scan takes a
	// serializable lock on the whole table. Index scans match real databases.
	setup, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, `DO $$ BEGIN
EXECUTE format('ALTER DATABASE %I SET enable_seqscan = off', current_database());
END $$`)
	require.NoError(t, err)
	require.NoError(t, setup.Close())
	store, err := OpenWithConfig(ctx, dsn, Config{Schema: schema, SchemaMode: SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

// Reading the newest clock value inside a writer's serializable snapshot takes
// a predicate lock on the clock index. Any concurrent event insert then
// conflicts with the writer, and PostgreSQL aborts one of them with 40001.
func TestCreateIssueTakesNoSerializableLockOnEventClock(t *testing.T) {
	store := openEventClockStore(t, "event_clock_predicate")
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)

	// PostgreSQL keeps a committed serializable transaction's predicate locks
	// while an overlapping serializable transaction stays open.
	overlap, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	require.NoError(t, err)
	t.Cleanup(func() { _ = overlap.Rollback() })
	var one int
	require.NoError(t, overlap.QueryRowContext(ctx, `SELECT 1`).Scan(&one))

	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Concurrent issue", Author: "example-actor",
	})
	require.NoError(t, err)

	var held []string
	rows, err := store.QueryContext(ctx, `SELECT DISTINCT relation::regclass::text FROM pg_locks
WHERE mode = 'SIReadLock'
  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND relation IN ('events'::regclass, 'idx_events_hlc'::regclass)`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var relation string
		require.NoError(t, rows.Scan(&relation))
		held = append(held, relation)
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, held, "the writer must not read the event clock under its serializable snapshot")
}

// A local event follows a supplied timestamp inserted earlier in its own
// transaction, even when that timestamp is ahead of wall time.
func TestEventClockFollowsSuppliedTimestampInSameTransaction(t *testing.T) {
	store := openEventClockStore(t, "event_clock_supplied")
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	future := db.EventHLCTimestamp{PhysicalMS: time.Now().Add(time.Hour).UnixMilli(), Counter: 7}

	var supplied, local db.Event
	require.NoError(t, store.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		supplied, err = store.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.metadata_updated", Actor: "example-actor", Payload: "{}", HLC: &future,
		})
		if err != nil {
			return err
		}
		local, err = store.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.metadata_updated", Actor: "example-actor", Payload: "{}",
		})
		return err
	}))
	assert.Equal(t, future, clockOf(supplied))
	assert.Greater(t, compareClock(clockOf(local), future), 0)

	// A later transaction reads the committed future timestamp.
	_, after, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "After the supplied timestamp", Author: "example-actor",
	})
	require.NoError(t, err)
	assert.Greater(t, compareClock(clockOf(after), clockOf(local)), 0)
}

func clockOf(event db.Event) db.EventHLCTimestamp {
	return db.EventHLCTimestamp{PhysicalMS: event.HLCPhysicalMS, Counter: event.HLCCounter}
}

func compareClock(a, b db.EventHLCTimestamp) int {
	return cmp.Or(cmp.Compare(a.PhysicalMS, b.PhysicalMS), cmp.Compare(a.Counter, b.Counter))
}
