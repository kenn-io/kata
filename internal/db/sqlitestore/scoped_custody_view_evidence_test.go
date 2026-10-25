package sqlitestore

import (
	"context"
	"database/sql"
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedSourceCustodyViewEvidence(t *testing.T) {
	dbtest.RunScopedSourceCustodyViewEvidence(t, func(t *testing.T) db.Storage { return openUIStore(t) }, func(ctx context.Context, storage db.Storage) (int64, error) {
		s := storage.(*Store)
		var charged int64
		err := s.relayTx(ctx, func(tx *sql.Tx) error {
			usage, err := db.ReadScopedSourceBudgetUsageTx(ctx, tx)
			if err != nil {
				return err
			}
			charged = usage.FamilyBytes["federation_source_custody"]
			return nil
		})
		return charged, err
	})
}
