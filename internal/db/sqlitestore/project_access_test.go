package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestProjectAccessConformance(t *testing.T) {
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.RunProjectAccessMergeIsolation(t, store)
	dbtest.RunProjectAccessConformance(t, store)
	dbtest.RunProjectAccessTokenEnrollment(t, store)
}
