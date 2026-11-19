package pgstore_test

import (
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestQuietFederationPollGrowth(t *testing.T) {
	var current *pgstore.Store
	dbtest.RunQuietFederationPollGrowth(t, func(t *testing.T) db.Storage {
		store, _ := openConformanceStore(t)
		store.SetMaxOpenConns(1)
		store.SetMaxIdleConns(1)
		t.Cleanup(func() { _ = store.Close() })
		current = store
		return store
	}, postgresBlockFetches(t, func() []*pgstore.Store { return []*pgstore.Store{current} }), 1)
}
