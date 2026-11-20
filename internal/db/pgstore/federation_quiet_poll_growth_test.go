package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kit/growthtest"
)

func TestQuietFederationPollGrowth(t *testing.T) {
	open, measure := postgresQuietPollGrowth(t)
	open, measure = vacuumFirst(t, open, measure)
	dbtest.RunQuietFederationPollGrowth(t, open, measure, 1)
}

func TestResumedFederationPollGrowth(t *testing.T) {
	open, measure := postgresQuietPollGrowth(t)
	dbtest.RunResumedFederationPollGrowth(t, open, measure, 1)
}

func TestRememberedFederationPollCost(t *testing.T) {
	open, measure := postgresQuietPollGrowth(t)
	dbtest.RunRememberedFederationPollCost(t, open, func(store db.Storage) {
		pgstore.ForgetFederationFeedForTest(store.(*pgstore.Store))
	}, measure, 1)
}

// postgresQuietPollGrowth opens single-connection stores and measures the
// blocks one steady-state poll reads.
func postgresQuietPollGrowth(t *testing.T) (func(*testing.T) db.Storage, growthtest.Measure) {
	var current *pgstore.Store
	return func(t *testing.T) db.Storage {
		store, _ := openConformanceStore(t)
		store.SetMaxOpenConns(1)
		store.SetMaxIdleConns(1)
		t.Cleanup(func() { _ = store.Close() })
		current = store
		return store
	}, postgresBlockFetches(t, func() []*pgstore.Store { return []*pgstore.Store{current} })
}

// vacuumFirst vacuums the store open last returned before each measurement.
// The block counts cover the whole database, so autovacuum reading freshly
// seeded tables during a sample would otherwise count as the poll's work.
func vacuumFirst(t *testing.T, open func(*testing.T) db.Storage, measure growthtest.Measure) (func(*testing.T) db.Storage, growthtest.Measure) {
	var current db.Storage
	return func(t *testing.T) db.Storage {
			current = open(t)
			return current
		}, func(run, reset func()) float64 {
			_, err := current.(*pgstore.Store).ExecContext(t.Context(), `VACUUM (ANALYZE)`)
			require.NoError(t, err)
			return measure(run, reset)
		}
}
