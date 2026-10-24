package pgstore_test

import (
	"testing"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestNativeCronTypedIdentity(t *testing.T) {
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { _ = store.Close() })
	t.Log("native identity backend=postgres")
	dbtest.RunNativeCronTypedIdentity(t, store)
}
