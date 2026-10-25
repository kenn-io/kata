package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/db"
)

func checkLinkEndpointsProjectAccessTx(ctx context.Context, tx *sql.Tx, issueIDs ...int64) error {
	if err := checkLinkProjectsAccessTx(ctx, tx, issueIDs...); err != nil {
		return err
	}
	for i := 1; i < len(issueIDs); i++ {
		if err := ensureRelayLinkBoundaryTx(ctx, tx, issueIDs[0], issueIDs[i], false); err != nil {
			return err
		}
	}
	return nil
}

// Existing-edge cleanup retains access and local root authority for archived peers.
func checkExistingLinkProjectAccessTx(ctx context.Context, tx *sql.Tx, link db.Link) error {
	if err := checkLinkProjectsAccessTx(ctx, tx, link.FromIssueID, link.ToIssueID); err != nil {
		return err
	}
	return ensureRelayLinkBoundaryTx(ctx, tx, link.FromIssueID, link.ToIssueID, true)
}

func checkParentReplacementProjectAccessTx(ctx context.Context, tx *sql.Tx, input db.ReplaceParentAndEventsParams) error {
	if err := checkLinkProjectsAccessTx(ctx, tx, input.Link.FromIssueID, input.ExpectedParentIssueID, input.Link.ToIssueID); err != nil {
		return err
	}
	if err := ensureRelayLinkBoundaryTx(ctx, tx, input.Link.FromIssueID, input.ExpectedParentIssueID, true); err != nil {
		return err
	}
	return ensureRelayLinkBoundaryTx(ctx, tx, input.Link.FromIssueID, input.Link.ToIssueID, false)
}

func checkLinkProjectsAccessTx(ctx context.Context, tx *sql.Tx, issueIDs ...int64) error {
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
