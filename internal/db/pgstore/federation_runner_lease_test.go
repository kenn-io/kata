package pgstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

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
	ctx := context.Background()
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
