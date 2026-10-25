package db

import (
	"context"
	"database/sql"
	"sort"
)

// RecordScopedFoldIssueMetadataTx consumes surviving writers from the existing
// native fold and actual successful event-store outcomes in this transaction.
// Already-stored history and subjects without root issuers create no state.
func RecordScopedFoldIssueMetadataTx(ctx context.Context, tx *sql.Tx, instanceUID string, projectID int64, writers map[string][]FoldClock, insertedEventUIDs []string) error {
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
	subjects := make([]string, 0, len(writers))
	for subject := range writers {
		subjects = append(subjects, subject)
	}
	sort.Strings(subjects)
	for _, subject := range subjects {
		for _, writer := range writers[subject] {
			if _, ok := inserted[writer.EventUID]; !ok {
				continue
			}
			if err := recordScopedPredicateIncarnationTx(ctx, tx, instanceUID, projectUID, subject, "issue.metadata", &writer); err != nil {
				return err
			}
			// Several newly accepted path writers still invalidate this one
			// subject predicate only once in the materialization transaction.
			break
		}
	}
	return nil
}
