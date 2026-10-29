package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type rollbackRetryConnector struct{}

func (rollbackRetryConnector) Connect(context.Context) (driver.Conn, error) {
	return rollbackRetryConn{}, nil
}

func (rollbackRetryConnector) Driver() driver.Driver { return rollbackRetryDriver{} }

type rollbackRetryDriver struct{}

func (rollbackRetryDriver) Open(string) (driver.Conn, error) { return rollbackRetryConn{}, nil }

type rollbackRetryConn struct{}

func (rollbackRetryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}
func (rollbackRetryConn) Close() error              { return nil }
func (rollbackRetryConn) Begin() (driver.Tx, error) { return rollbackRetryTx{}, nil }
func (rollbackRetryConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return rollbackRetryTx{}, nil
}

type rollbackRetryTx struct{}

func (rollbackRetryTx) Commit() error   { return nil }
func (rollbackRetryTx) Rollback() error { return nil }

func TestIsTransientUsesRetrySafeSQLStates(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"40001", "40P01", "55P03"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			err := &pgconn.PgError{Code: code}
			assert.True(t, IsTransient(err))
			assert.True(t, IsTransient(errors.Join(errors.New("operation failed"), err)))
		})
	}
	for _, code := range []string{"23505", "23503", "57014", "08006"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			assert.False(t, IsTransient(&pgconn.PgError{Code: code}))
		})
	}
	assert.False(t, IsTransient(errors.New("serialization failure")))
}

func TestWithTxFinishesAfterRollbackBeforeTransientRetry(t *testing.T) {
	ctx := context.Background()
	database := sql.OpenDB(rollbackRetryConnector{})
	t.Cleanup(func() { _ = database.Close() })
	store := &Store{DB: database}
	transient := errors.Join(errors.New("peer access check rejected the transaction"), &pgconn.PgError{Code: "40001"})
	var attempts, callbacks, callbacksAtRetry int
	err := store.withSerializableTx(ctx, func(*sql.Tx) error {
		attempts++
		if attempts == 1 {
			return db.AfterTransactionRollback(transient, func(context.Context) error {
				callbacks++
				return nil
			})
		}
		callbacksAtRetry = callbacks
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, 1, callbacksAtRetry, "host cleanup must finish before the retry begins")
	assert.Equal(t, 1, callbacks)
}

func TestMapSQLErrorUsesDomainSentinelWithoutLeakingDetail(t *testing.T) {
	t.Parallel()

	const sensitiveDetail = "Key (from_issue_id)=(private-row-value) already exists"
	pgErr := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "uniq_one_parent_per_child",
		Detail:         sensitiveDetail,
	}
	mapped := mapSQLError(pgErr, map[string]error{
		"uniq_one_parent_per_child": db.ErrParentAlreadySet,
	})
	assert.ErrorIs(t, mapped, db.ErrParentAlreadySet)
	assert.NotContains(t, mapped.Error(), sensitiveDetail)
	assert.Contains(t, mapped.Error(), "uniq_one_parent_per_child")
	var leaked *pgconn.PgError
	assert.False(t, errors.As(mapped, &leaked), "sanitized errors must not expose server diagnostics")

	unknown := mapSQLError(&pgconn.PgError{
		Code:   "23514",
		Detail: sensitiveDetail,
	}, nil)
	assert.NotContains(t, unknown.Error(), sensitiveDetail)
	var sqlErr *SQLError
	require.ErrorAs(t, unknown, &sqlErr)
	assert.Equal(t, "23514", sqlErr.Code)
	assert.False(t, errors.As(unknown, &leaked), "unknown constraints must remain sanitized")

	assert.ErrorIs(t, mapSQLError(sql.ErrNoRows, nil), db.ErrNotFound)

	authorizationUnavailable := errors.New("authorization unavailable")
	joined := errors.Join(authorizationUnavailable, pgErr)
	for _, input := range []error{joined, mapSQLError(joined, nil)} {
		mapped := mapSQLError(input, map[string]error{
			"uniq_one_parent_per_child": db.ErrParentAlreadySet,
		})
		assert.ErrorIs(t, mapped, authorizationUnavailable)
		assert.ErrorIs(t, mapped, db.ErrParentAlreadySet)
		assert.False(t, errors.As(mapped, &leaked))
	}
}

func TestTransactionHelpersRollbackRetryAndReserveIdentityValues(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema:     "transaction_store",
		SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.ExecContext(ctx, `CREATE TABLE transaction_probe (value integer PRIMARY KEY)`)
	require.NoError(t, err)

	marker := errors.New("abort transaction")
	err = store.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var isolation string
		require.NoError(t, tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation))
		assert.Equal(t, "serializable", isolation)
		_, execErr := tx.ExecContext(ctx, `INSERT INTO transaction_probe(value) VALUES (1)`)
		require.NoError(t, execErr)
		return marker
	})
	assert.ErrorIs(t, err, marker)
	var count int
	require.NoError(t, store.QueryRowContext(ctx, `SELECT COUNT(*) FROM transaction_probe`).Scan(&count))
	assert.Zero(t, count, "callback errors must roll back all writes")

	attempts := 0
	err = store.withRepeatableReadTx(ctx, func(tx *sql.Tx) error {
		attempts++
		var isolation string
		require.NoError(t, tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation))
		assert.Equal(t, "repeatable read", isolation)
		if attempts < 3 {
			return &pgconn.PgError{Code: "40001"}
		}
		_, execErr := tx.ExecContext(ctx, `INSERT INTO transaction_probe(value) VALUES (2)`)
		return execErr
	})
	require.NoError(t, err)
	assert.Equal(t, 3, attempts)
	require.NoError(t, store.QueryRowContext(ctx, `SELECT COUNT(*) FROM transaction_probe`).Scan(&count))
	assert.Equal(t, 1, count, "only the successful transaction attempt may commit")

	err = store.withSerializableTx(ctx, func(tx *sql.Tx) error {
		first, reserveErr := store.reserveIdentityValue(ctx, tx, "events", "id")
		require.NoError(t, reserveErr)
		second, reserveErr := store.reserveIdentityValue(ctx, tx, "events", "id")
		require.NoError(t, reserveErr)
		assert.Equal(t, first+1, second)
		return nil
	})
	require.NoError(t, err)

	_, err = store.reserveIdentityValue(ctx, nil, `events; DROP TABLE events`, "id")
	assert.Error(t, err)
}

func TestPurgeWaitsForEventSequenceFence(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "event_fence_store", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "event-fence-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "purge behind fence", Author: "conformance-agent",
	})
	require.NoError(t, err)

	blocker, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	require.NoError(t, lockEventSequenceTx(ctx, blocker))
	reserved, err := store.reserveIdentityValue(ctx, blocker, "events", "id")
	require.NoError(t, err)

	type purgeResult struct {
		log db.PurgeLog
		err error
	}
	done := make(chan purgeResult, 1)
	go func() {
		log, purgeErr := store.PurgeIssue(ctx, issue.ID, "conformance-agent", nil)
		done <- purgeResult{log: log, err: purgeErr}
	}()
	select {
	case result := <-done:
		require.Failf(t, "purge bypassed event fence", "result=%+v err=%v", result.log, result.err)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // absence window; the blocker transaction holds the event fence, so the purge must not finish
	}
	require.NoError(t, blocker.Commit())
	result := <-done
	require.NoError(t, result.err)
	require.NotNil(t, result.log.PurgeResetAfterEventID)
	assert.Greater(t, *result.log.PurgeResetAfterEventID, reserved)
}

func TestTransactionFencePostgresFailureKeepsErrorCategory(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "fence_error_store", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	authorizationUnavailable := errors.New("authorization unavailable")

	for _, failFinalization := range []bool{false, true} {
		name := "authorization"
		if failFinalization {
			name = "finalization"
		}
		t.Run(name, func(t *testing.T) {
			fence := func(ctx context.Context, tx db.Transaction) error {
				_, queryErr := tx.ExecContext(ctx, `SELECT 'private-row-value'::integer`)
				cause := errors.Join(authorizationUnavailable, queryErr)
				if failFinalization {
					return db.AfterTransactionRollback(cause, func(context.Context) error {
						return errors.New("host recording failed")
					})
				}
				return cause
			}
			_, err := store.CreateProject(db.WithTransactionFence(ctx, fence), "rejected-project")
			require.Error(t, err)
			assert.ErrorIs(t, err, authorizationUnavailable)
			assert.Equal(t, failFinalization, errors.Is(err, db.ErrTransactionFinalizationFailed))
			assert.NotContains(t, err.Error(), "private-row-value")
			_, exposesDiagnostics := errors.AsType[*pgconn.PgError](err)
			assert.False(t, exposesDiagnostics)
		})
	}
	_, err = store.ProjectByName(ctx, "rejected-project")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// R5: a receipt reset and ordinary source events share commit ordering as well
// as an identity sequence. Otherwise a later event can hide a late proof reset.
func TestAttributionUIResetWaitsForEventSequenceFence(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{Schema: "receipt_fence_store", SchemaMode: SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "receipt-fence-project")
	require.NoError(t, err)
	blocker, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	require.NoError(t, lockEventSequenceTx(ctx, blocker))
	reserved, err := store.reserveIdentityValue(ctx, blocker, "events", "id")
	require.NoError(t, err)
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		done <- store.withSerializableTx(ctx, func(tx *sql.Tx) error {
			return reserveAttributionUIResetTx(ctx, tx, project.UID)
		})
	}()
	<-started
	select {
	case err := <-done:
		require.NoError(t, err)
		t.Fatal("receipt reset committed while the source event sequence fence was held")
	case <-time.After(time.Second):
	}
	require.NoError(t, blocker.Commit())
	require.NoError(t, <-done)
	cursor, err := store.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Greater(t, cursor, reserved)
}
