package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

func checkLinkEndpointsProjectAccessTx(ctx context.Context, tx *sql.Tx, issueIDs ...int64) error {
	seen := make(map[int64]struct{}, len(issueIDs))
	for _, issueID := range issueIDs {
		if _, ok := seen[issueID]; ok {
			continue
		}
		seen[issueID] = struct{}{}
		var projectUID string
		err := tx.QueryRowContext(ctx, `
			SELECT p.uid
			  FROM issues i JOIN projects p ON p.id = i.project_id
			 WHERE i.id = ?`, issueID).Scan(&projectUID)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := db.CheckProjectAccessTransaction(ctx, tx, projectUID); err != nil {
			return err
		}
	}
	return nil
}
