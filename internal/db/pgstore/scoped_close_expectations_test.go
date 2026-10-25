package pgstore

import (
	"go.kenn.io/kata/internal/db/dbtest"
	"testing"
)

func TestScopedClosePreparation(t *testing.T) {
	dbtest.RunScopedClosePreparation(t, openUIStore(t))
}
