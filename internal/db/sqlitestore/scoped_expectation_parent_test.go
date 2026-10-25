package sqlitestore

import (
	"testing"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedExpectationCurrentParent(t *testing.T) {
	dbtest.RunScopedExpectationCurrentParent(t, openUIStore(t))
}
