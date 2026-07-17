package federation_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
)

func TestPostgresFederationRunnersElectOneLeaderPerSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	const schema = "federation_runner_lease"
	firstStore, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = firstStore.Close() })
	secondStore, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = secondStore.Close() })

	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- (&federation.Runner{DB: firstStore, Interval: time.Hour}).Run(firstCtx)
	}()
	var firstPID int
	require.Eventually(t, func() bool {
		firstPID, err = federationRunnerLeasePID(ctx, firstStore.DB, schema)
		return err == nil
	}, 5*time.Second, 25*time.Millisecond)

	secondCtx, cancelSecond := context.WithCancel(ctx)
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- (&federation.Runner{DB: secondStore, Interval: time.Hour}).Run(secondCtx)
	}()
	<-secondStarted
	assert.Never(t, func() bool {
		pid, queryErr := federationRunnerLeasePID(ctx, firstStore.DB, schema)
		return queryErr == nil && pid != firstPID
	}, 300*time.Millisecond, 25*time.Millisecond,
		"standby runner must not enter a poll cycle while the leader holds the schema lease")

	cancelFirst()
	assert.ErrorIs(t, <-firstDone, context.Canceled)
	var secondPID int
	require.Eventually(t, func() bool {
		secondPID, err = federationRunnerLeasePID(ctx, secondStore.DB, schema)
		return err == nil && secondPID != firstPID
	}, 5*time.Second, 25*time.Millisecond,
		"standby runner must take leadership after the first runner exits")

	cancelSecond()
	assert.ErrorIs(t, <-secondDone, context.Canceled)
	require.Eventually(t, func() bool {
		_, queryErr := federationRunnerLeasePID(ctx, firstStore.DB, schema)
		return queryErr == sql.ErrNoRows
	}, 5*time.Second, 25*time.Millisecond,
		"the final runner must release the schema lease on shutdown")
}

func TestPostgresFederationRunnerRecoversAfterLeaseSessionLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	const schema = "federation_runner_failover"
	firstStore, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = firstStore.Close() })
	secondStore, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = secondStore.Close() })

	firstCtx, cancelFirst := context.WithCancel(ctx)
	secondCtx, cancelSecond := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- (&federation.Runner{DB: firstStore, Interval: 25 * time.Millisecond}).Run(firstCtx)
	}()
	var firstPID int
	require.Eventually(t, func() bool {
		firstPID, err = federationRunnerLeasePID(ctx, firstStore.DB, schema)
		return err == nil
	}, 5*time.Second, 25*time.Millisecond)
	go func() {
		secondDone <- (&federation.Runner{DB: secondStore, Interval: 25 * time.Millisecond}).Run(secondCtx)
	}()

	var terminated bool
	require.NoError(t, firstStore.QueryRowContext(ctx,
		`SELECT pg_catalog.pg_terminate_backend($1)`, firstPID).Scan(&terminated))
	require.True(t, terminated)
	var successorPID int
	require.Eventually(t, func() bool {
		successorPID, err = federationRunnerLeasePID(ctx, secondStore.DB, schema)
		return err == nil && successorPID != firstPID
	}, 5*time.Second, 25*time.Millisecond,
		"one runner must reacquire leadership after PostgreSQL drops the lease session")

	select {
	case runErr := <-firstDone:
		require.FailNow(t, "former leader exited instead of rejoining standby", runErr.Error())
	case runErr := <-secondDone:
		require.FailNow(t, "standby exited during lease failover", runErr.Error())
	case <-time.After(300 * time.Millisecond):
	}

	cancelFirst()
	cancelSecond()
	assert.ErrorIs(t, <-firstDone, context.Canceled)
	assert.ErrorIs(t, <-secondDone, context.Canceled)
}

func federationRunnerLeasePID(ctx context.Context, database *sql.DB, schema string) (int, error) {
	var pid int
	err := database.QueryRowContext(ctx, `
		SELECT pid
		  FROM pg_catalog.pg_locks
		 WHERE locktype = 'advisory'
		   AND granted
		   AND classid = (pg_catalog.hashtext(pg_catalog.current_database())::bigint & 4294967295)
		   AND objid = (pg_catalog.hashtext($1)::bigint & 4294967295)
		 LIMIT 1`, "kata:federation:runner:"+schema).Scan(&pid)
	return pid, err
}
