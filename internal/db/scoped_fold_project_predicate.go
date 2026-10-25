package db

import (
	"context"
	"database/sql"
)

// RecordScopedFoldProjectMetadataTx consumes surviving writers from the existing
// native fold and actual successful event-store outcomes in this transaction.
// It composes once for this native project and never creates root issuer state.
func RecordScopedFoldProjectMetadataTx(ctx context.Context, tx *sql.Tx, instanceUID string, projectID int64, writers map[string][]FoldClock, insertedEventUIDs []string) error {
	if len(insertedEventUIDs) == 0 || len(writers) == 0 {
		return nil
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID); err != nil {
		return err
	}
	inserted := make(map[string]struct{}, len(insertedEventUIDs))
	for _, uid := range insertedEventUIDs {
		inserted[uid] = struct{}{}
	}
	for _, writer := range writers[projectUID] {
		if _, ok := inserted[writer.EventUID]; !ok {
			continue
		}
		return recordScopedPredicateWriterTx(ctx, tx, instanceUID, projectUID, projectUID, "project.metadata", projectUID, &writer)
	}
	return nil
}
