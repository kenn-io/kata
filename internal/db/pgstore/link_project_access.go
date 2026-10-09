package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/db"
)

func checkLinkEndpointsProjectAccessTx(ctx context.Context, tx *sql.Tx, issueIDs ...int64) error {
	ids := slices.Clone(issueIDs)
	slices.Sort(ids)
	last := int64(0)
	for i, issueID := range ids {
		if i > 0 && issueID == last {
			continue
		}
		last = issueID
		var projectUID string
		err := tx.QueryRowContext(ctx, `
			SELECT p.uid
			  FROM issues i JOIN projects p ON p.id = i.project_id
			 WHERE i.id = $1
			 FOR SHARE OF i, p`, issueID).Scan(&projectUID)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		if err := db.CheckProjectAccessTransaction(ctx, tx, projectUID); err != nil {
			return err
		}
	}
	return nil
}
