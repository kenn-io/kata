package pgstore

import (
	"context"
	"database/sql"
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedBindingBudgetActivation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta int64
	}{{"cap_minus_one", -1}, {"at_cap", 0}, {"cap_plus_one", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			probe, root := openUIStore(t), openUIStore(t)
			dbtest.RunScopedBindingBudgetActivation(t, probe, root, tc.delta, func(ctx context.Context, key db.ScopedViewKey) error {
				return root.scopedReadTransaction(ctx, func(tx *sql.Tx) error {
					_, err := root.scopedAuthorityTx(ctx, tx, key, "pull")
					return err
				})
			})
		})
	}
}
