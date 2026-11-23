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

func TestFederationPollWakeReachesAnotherProcessOnTheSameSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	first := openPollWakeStore(t, ctx, dsn, pgstore.DefaultSchema)
	second := openPollWakeStore(t, ctx, dsn, pgstore.DefaultSchema)
	firstWakes := listenForPollWakes(t, ctx, first)
	secondWakes := listenForPollWakes(t, ctx, second)

	require.NoError(t, first.PublishFederationPollWake(ctx, "hub-a 5"))
	require.Equal(t, "hub-a 5", nextPollWake(t, ctx, secondWakes))
	require.NoError(t, second.PublishFederationPollWake(ctx, "hub-b 6 01JWAKEMOVEDFROMPROJECT000"))
	require.Equal(t, "hub-a 5", nextPollWake(t, ctx, firstWakes))
	require.Equal(t, "hub-b 6 01JWAKEMOVEDFROMPROJECT000", nextPollWake(t, ctx, firstWakes))
}

func TestFederationPollWakeStaysWithinItsSchema(t *testing.T) {
	t.Run("installation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		const schema = "wake_probe_schema"
		dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
		t.Cleanup(cleanup)
		otherDSN, otherCleanup := pgstoretest.NewDatabase(t, ctx)
		t.Cleanup(otherCleanup)
		store := openPollWakeStore(t, ctx, dsn, schema)
		otherInstallation := openPollWakeStore(t, ctx, otherDSN, schema)

		channel, err := pgstore.FederationPollWakeChannelForTest(store)
		require.NoError(t, err)
		otherChannel, err := pgstore.FederationPollWakeChannelForTest(otherInstallation)
		require.NoError(t, err)
		require.NotEqual(t, otherChannel, channel, "installations sharing a schema name must not share a channel")
	})
	t.Run("schema", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
		t.Cleanup(cleanup)
		kata := openPollWakeStore(t, ctx, dsn, pgstore.DefaultSchema)
		other := openPollWakeStore(t, ctx, dsn, "kata_other")
		// An import into the second schema can copy the first installation's
		// identity; the schemas must stay apart even then.
		_, err := other.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key='instance_uid'`, kata.InstanceUID())
		require.NoError(t, err)
		require.NoError(t, other.RefreshInstanceUID(ctx))
		otherWakes := listenForPollWakes(t, ctx, other)

		require.NoError(t, kata.PublishFederationPollWake(ctx, "hub-a 7"))
		require.NoError(t, other.PublishFederationPollWake(ctx, "hub-b 8"))
		require.Equal(t, "hub-b 8", nextPollWake(t, ctx, otherWakes), "a wake crossed into another schema")
	})
}

//nolint:revive // t is the canonical first arg for testing helpers
func openPollWakeStore(t *testing.T, ctx context.Context, dsn, schema string) *pgstore.Store {
	t.Helper()
	config := pgstore.DefaultConfig()
	config.Schema = schema
	store, err := pgstore.OpenWithConfig(ctx, dsn, config)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// listenForPollWakes runs the store's wake listener until the test ends and
// returns the payloads it delivers.
//
//nolint:revive // t is the canonical first arg for testing helpers
func listenForPollWakes(t *testing.T, ctx context.Context, store *pgstore.Store) <-chan string {
	t.Helper()
	ctx, stop := context.WithCancel(ctx)
	wakes := make(chan string, 16)
	listening := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = store.ListenFederationPollWake(ctx, func() { close(listening) }, func(payload string) {
			select {
			case wakes <- payload:
			case <-ctx.Done():
			}
		})
	}()
	t.Cleanup(func() { stop(); <-done })
	select {
	case <-listening:
	case <-ctx.Done():
		t.Fatal("the wake listener never started")
	}
	return wakes
}

//nolint:revive // t is the canonical first arg for testing helpers
func nextPollWake(t *testing.T, ctx context.Context, wakes <-chan string) string {
	t.Helper()
	select {
	case payload := <-wakes:
		return payload
	case <-ctx.Done():
		t.Fatal("no poll wake arrived")
		return ""
	}
}

func TestFederationPollWakeSecretConcurrentBootstrapAndExport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store := openPollWakeStore(t, ctx, dsn, pgstore.DefaultSchema)
	second := openPollWakeStore(t, ctx, dsn, pgstore.DefaultSchema)
	const count = 16
	results := make(chan []byte, count)
	errs := make(chan error, count)
	for i := range count {
		go func() {
			current := store
			if i%2 == 1 {
				current = second
			}
			secret, err := current.FederationPollWakeSecret(ctx)
			results <- secret
			errs <- err
		}()
	}
	var first []byte
	for range count {
		require.NoError(t, <-errs)
		secret := <-results
		require.Len(t, secret, 32)
		if first == nil {
			first = secret
		}
		require.Equal(t, first, secret)
	}
	for meta, err := range store.ExportMeta(ctx) {
		require.NoError(t, err)
		require.NotEqual(t, "federation_poll_wake_secret", meta.Key)
	}
}
