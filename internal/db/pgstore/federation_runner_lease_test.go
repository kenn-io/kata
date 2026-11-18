package pgstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationRunnerLeaseRetryClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad connection", err: driver.ErrBadConn, want: true},
		{name: "connection failure", err: &pgconn.PgError{Code: "08006"}, want: true},
		{name: "database starting", err: &pgconn.PgError{Code: "57P03"}, want: true},
		{name: "permission denied", err: &pgconn.PgError{Code: "42501"}, want: false},
		{name: "closed pool", err: errors.New("sql: database is closed"), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isFederationRunnerLeaseRetryable(tc.err))
		})
	}
}

func TestFederationRunnerLeaseReturnsPermanentConnectionFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := Open(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	release, err := store.AcquireFederationRunnerLease(ctx)
	assert.Nil(t, release)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserve postgres federation runner lease connection")
}

func TestFederationRunnerLeaseReleaseAfterQueryCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	release, err := store.AcquireFederationRunnerLease(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })
	conn := store.federationLease.conn
	var pid int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))

	queryCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	queryDone := make(chan error, 1)
	go func() {
		// Keep a real query in flight so cancellation closes the lease session,
		// just as cancellation during the runner's lock check can do.
		_, queryErr := conn.ExecContext(queryCtx, `SELECT pg_sleep(60)`)
		queryDone <- queryErr
	}()
	//nolint:kennlint // sleeptest: wait for PostgreSQL to enter the real query before canceling it.
	require.Eventually(t, func() bool {
		var sleeping bool
		queryErr := store.QueryRowContext(ctx, `
			SELECT wait_event = 'PgSleep' FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&sleeping)
		return queryErr == nil && sleeping
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-queryDone, context.Canceled)

	require.NoError(t, release())
	require.NoError(t, release(), "release is idempotent")
	//nolint:kennlint // sleeptest: PostgreSQL releases session locks after processing the disconnect.
	require.Eventually(t, func() bool {
		var held bool
		queryErr := store.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND locktype = 'advisory')`, pid).Scan(&held)
		return queryErr == nil && !held
	}, 5*time.Second, 10*time.Millisecond)
}
