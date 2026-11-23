package db

import (
	"context"
	"database/sql"
)

// CheckRetainedDiscussionMove refuses moves whose comment identities cannot be
// proven against the replacement enrollment's hub.
func CheckRetainedDiscussionMove(ctx context.Context, tx *sql.Tx, projectID, issueID int64, issueUID, instanceUID string) error {
	var unverified bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM federation_bindings b
		WHERE b.project_id=$1 AND b.role='spoke' AND b.enabled=1 AND b.push_enabled=1
		  AND b.retained_through_event_id>0
		  AND EXISTS (SELECT 1 FROM comments c WHERE c.issue_id=$2)
		  AND EXISTS (SELECT 1 FROM events e WHERE e.project_id=$1 AND e.issue_uid=$3
		    AND `+LocalInstanceOriginSQL("e.origin_instance_uid", "$4")+`
		    AND e.id<=b.retained_through_event_id AND (e.origin_instance_uid<>$4 OR e.actor<>b.bound_actor))
	)`, projectID, issueID, issueUID, instanceUID).Scan(&unverified)
	if err != nil {
		return err
	}
	if unverified {
		return ErrRetainedDiscussionMove
	}
	return nil
}
