package sqlitestore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kit/growthtest"
)

func TestQuietFederationPollGrowth(t *testing.T) {
	open, measure := sqliteQuietPollGrowth(t)
	dbtest.RunQuietFederationPollGrowth(t, open, measure, 1)
}

func TestResumedFederationPollGrowth(t *testing.T) {
	open, measure := sqliteQuietPollGrowth(t)
	dbtest.RunResumedFederationPollGrowth(t, open, measure, 1)
}

// sqliteQuietPollGrowth opens stores whose page reads can be counted and
// measures the pages one steady-state poll reads.
func sqliteQuietPollGrowth(t *testing.T) (func(*testing.T) db.Storage, growthtest.Measure) {
	var current *sql.DB
	pages := func(reset bool) int {
		conn, err := current.Conn(t.Context())
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }()
		return sqlitePageFetches(t, conn, reset)
	}
	return func(t *testing.T) db.Storage {
			store, path := openTestDBWithPath(t)
			// The write-queue wrapper doesn't expose the driver's page counters.
			raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			raw.SetMaxOpenConns(1)
			current = raw
			measured := *store
			measured.DB = raw
			return &measured
		}, func(run, reset func()) float64 {
			// Count database work instead of elapsed time, which varies with CI load.
			// The first poll after the reset is the steady state.
			reset()
			run()
			reset()
			pages(true)
			run()
			return float64(pages(false))
		}
}
