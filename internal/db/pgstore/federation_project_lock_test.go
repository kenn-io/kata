package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationProjectAdvisoryLockCoordinatesStores(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	first, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	releaseShared, err := first.AcquireFederationProjectSharedLock(ctx, 42)
	require.NoError(t, err)
	exclusiveAcquired := make(chan func(), 1)
	go func() {
		release, lockErr := second.AcquireFederationProjectExclusiveLock(ctx, 42)
		if lockErr != nil {
			exclusiveAcquired <- nil
			return
		}
		exclusiveAcquired <- release
	}()

	select {
	case <-exclusiveAcquired:
		t.Fatal("exclusive project lock acquired while another store held a shared lock")
	case <-time.After(100 * time.Millisecond):
	}
	releaseShared()

	var releaseExclusive func()
	select {
	case releaseExclusive = <-exclusiveAcquired:
		require.NotNil(t, releaseExclusive)
	case <-ctx.Done():
		t.Fatal("exclusive project lock did not acquire after shared lock release")
	}

	sharedAcquired := make(chan func(), 1)
	go func() {
		release, lockErr := first.AcquireFederationProjectSharedLock(ctx, 42)
		if lockErr != nil {
			sharedAcquired <- nil
			return
		}
		sharedAcquired <- release
	}()
	select {
	case <-sharedAcquired:
		t.Fatal("shared project lock acquired while another store held an exclusive lock")
	case <-time.After(100 * time.Millisecond):
	}
	releaseExclusive()

	select {
	case release := <-sharedAcquired:
		require.NotNil(t, release)
		release()
	case <-ctx.Done():
		t.Fatal("shared project lock did not acquire after exclusive lock release")
	}
}
