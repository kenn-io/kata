package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

// Owner export must release its current cursor and stop opening later families
// when the consumer stops, or when a query/scan fails.
func TestScopedExportCursorLifetime(t *testing.T) {
	store := openUIStore(t)
	f := dbtest.NewScopedAuthorityFixture(t, store)
	_, err := store.CreateScopedRelayEnrollment(t.Context(), f.Params)
	require.NoError(t, err)

	t.Run("consumer stops", func(t *testing.T) {
		var cursor *sql.Rows
		queries := 0
		query := func(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
			queries++
			var err error
			cursor, err = store.QueryContext(ctx, statement, args...)
			return cursor, err
		}
		seen := 0
		for record, err := range db.ExportScopedStateSQL(t.Context(), query) {
			require.NoError(t, err)
			require.IsType(t, &db.ScopedViewExport{}, record)
			seen++
			break
		}
		require.Equal(t, 1, seen)
		require.Equal(t, 1, queries, "later families must remain unopened")
		_, err := cursor.Columns()
		require.Error(t, err, "consumer exit must close the SQL cursor")
	})

	t.Run("consumer cancels while stopping", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var cursor *sql.Rows
		queries := 0
		query := func(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
			queries++
			var err error
			cursor, err = store.QueryContext(ctx, statement, args...)
			return cursor, err
		}
		seen := 0
		require.NotPanics(t, func() {
			for record, err := range db.ExportScopedStateSQL(ctx, query) {
				require.NoError(t, err)
				require.IsType(t, &db.ScopedViewExport{}, record)
				seen++
				cancel()
				require.Eventually(t, func() bool {
					return errors.Is(cursor.Err(), context.Canceled)
				}, time.Second, time.Millisecond, "native cursor must observe request cancellation")
				break
			}
		}, "a stopped consumer cannot receive another error yield")
		require.Equal(t, 1, seen)
		require.Equal(t, 1, queries)
		_, err := cursor.Columns()
		require.Error(t, err)
	})

	t.Run("query fails", func(t *testing.T) {
		failure := errors.New("owner snapshot query failed")
		queries := 0
		query := func(context.Context, string, ...any) (*sql.Rows, error) {
			queries++
			return nil, failure
		}
		seen := 0
		for record, err := range db.ExportScopedStateSQL(t.Context(), query) {
			require.Nil(t, record)
			require.ErrorIs(t, err, failure)
			require.Contains(t, err.Error(), "export federation_scoped_views:")
			seen++
		}
		require.Equal(t, 1, seen)
		require.Equal(t, 1, queries)
	})

	t.Run("scan fails", func(t *testing.T) {
		var cursor *sql.Rows
		queries := 0
		query := func(ctx context.Context, _ string, _ ...any) (*sql.Rows, error) {
			queries++
			var err error
			cursor, err = store.QueryContext(ctx, "SELECT 1")
			return cursor, err
		}
		seen := 0
		for record, err := range db.ExportScopedStateSQL(t.Context(), query) {
			require.Nil(t, record)
			require.Error(t, err)
			require.Contains(t, err.Error(), "export federation_scoped_views:")
			_, closeErr := cursor.Columns()
			require.Error(t, closeErr, "error must be delivered after closing the cursor")
			seen++
		}
		require.Equal(t, 1, seen)
		require.Equal(t, 1, queries)
	})
}
