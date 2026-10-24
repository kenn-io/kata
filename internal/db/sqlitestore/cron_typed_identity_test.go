package sqlitestore_test

import (
	"testing"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestNativeCronTypedIdentity(t *testing.T) {
	store := openTestDB(t)
	t.Log("native identity backend=sqlite")
	dbtest.RunNativeCronTypedIdentity(t, store)
}
