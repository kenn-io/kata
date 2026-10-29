package db

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// ErrEvidenceTargetUnavailable reports a duplicate or superseding issue that
// was deleted or purged after the close request validated it.
var ErrEvidenceTargetUnavailable = errors.New("evidence target issue is no longer available")

// CheckEvidenceTargetsTx rechecks resolved duplicate-of and superseded-by
// targets inside the close transaction. A target may move or disappear after
// request validation; its current project must still be authorized.
func CheckEvidenceTargetsTx(ctx context.Context, tx Transaction, evidence []Evidence) error {
	for _, item := range evidence {
		if item.Type != "duplicate-of" && item.Type != "superseded-by" || !uid.Valid(item.IssueRef) {
			continue
		}
		var projectUID string
		var deleted bool
		err := tx.QueryRowContext(ctx, `SELECT p.uid, i.deleted_at IS NOT NULL FROM issues i
 JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, item.IssueRef).Scan(&projectUID, &deleted)
		if errors.Is(err, sql.ErrNoRows) || err == nil && deleted {
			return ErrEvidenceTargetUnavailable
		}
		if err != nil {
			return err
		}
		if err := CheckProjectAccessTransaction(ctx, tx, projectUID); err != nil {
			return err
		}
	}
	return nil
}
