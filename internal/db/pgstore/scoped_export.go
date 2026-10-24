package pgstore

import (
	"context"
	"go.kenn.io/kata/internal/db"
	"iter"
)

// ExportScopedState is called only for full owner snapshots. Historical owed
// rows survive binding revocation, project retirement and ordinary v1 reset.
func (s *Store) ExportScopedState(ctx context.Context) iter.Seq2[db.ImportRecord, error] {
	return db.ExportScopedStateSQL(ctx, s.exportQueryContext)
}
