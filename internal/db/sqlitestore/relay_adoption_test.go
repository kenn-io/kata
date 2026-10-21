package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationAdoptionPersistence(t *testing.T) {
	s, path := openTestDBWithPath(t)
	dbtest.RunFederationAdoptionPersistence(t, s, func() db.Storage {
		again, err := sqlitestore.Open(t.Context(), path)
		require.NoError(t, err)
		return again
	})
}
