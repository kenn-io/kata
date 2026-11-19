package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

func TestFederationPollWakeReportsClosedAfterStoreClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)

	listening := make(chan struct{})
	listened := make(chan error, 1)
	go func() {
		listened <- store.ListenFederationPollWake(ctx, func() { close(listening) }, func(string) {})
	}()
	select {
	case <-listening:
	case <-ctx.Done():
		t.Fatal("the listener never started")
	}
	require.NoError(t, store.Close())

	require.ErrorIs(t, <-listened, db.ErrFederationPollWakeClosed)
	require.ErrorIs(t, store.PublishFederationPollWake(ctx, "wake"), db.ErrFederationPollWakeClosed)
	require.ErrorIs(t, store.ListenFederationPollWake(ctx, func() {}, func(string) {}), db.ErrFederationPollWakeClosed)
}
