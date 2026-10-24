package sqlitestore

import (
	"context"
	"database/sql"
	"go.kenn.io/kata/internal/db"
)

func importScopedState(ctx context.Context, tx *sql.Tx, record db.ImportRecord) error {
	return db.ReplayScopedRecord(ctx, tx, record)
}
