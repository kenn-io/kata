package sqlitestore

import (
	"context"
	"database/sql"
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedDaemonBudgetAccountingGeometry(t *testing.T) {
	root := openUIStore(t)
	dbtest.RunScopedDaemonBudgetAccountingGeometry(t, root, func(ctx context.Context, key db.ScopedViewKey) error {
		return root.scopedReadTransaction(ctx, func(tx *sql.Tx) error {
			_, err := root.scopedAuthorityTx(ctx, tx, key, "pull")
			return err
		})
	})
}
