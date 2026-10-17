package db

import (
	"context"
)

// CheckFederationProjectIdentityRetirement protects only former identities
// still referenced by another joined project's portable issue history. Archive
// and leave retain the identity; purge and merge must wait for those streams to
// detach. Local history covered by an adoption baseline creates no dependency.
func CheckFederationProjectIdentityRetirement(ctx context.Context, tx federationHistoryQuery, project Project, snapshotExpression string) error {
	cutoffs := map[string]int64{}
	var afterID int64
	for {
		const limit = 1000
		rows, err := tx.QueryContext(ctx, `SELECT e.id,e.issue_uid,e.project_id,e.type,e.payload FROM events e
 WHERE (e.project_id=$1 OR e.type='issue.moved') AND e.id>$2 AND e.issue_uid IN (
 SELECT i.uid FROM issues i JOIN federation_bindings b ON b.project_id=i.project_id WHERE i.project_id<>$1)
 ORDER BY e.id LIMIT $3`, project.ID, afterID, limit)
		if err != nil {
			return err
		}
		var events []Event
		for rows.Next() {
			var event Event
			var issueUID, payload string
			if err := rows.Scan(&event.ID, &issueUID, &event.ProjectID, &event.Type, &payload); err != nil {
				_ = rows.Close()
				return err
			}
			event.IssueUID, event.Payload = new(issueUID), payload
			events = append(events, event)
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := loadFederationMoveHistoryCutoffs(ctx, tx, events, cutoffs, snapshotExpression); err != nil {
			return err
		}
		for _, event := range events {
			afterID = event.ID
			if event.ID < cutoffs[*event.IssueUID] {
				continue
			}
			if event.ProjectID == project.ID {
				return ErrFederationProjectIdentityRequired
			}
			payload := PayloadMap([]byte(event.Payload))
			if payload["snapshot"] == nil {
				continue // Bare local moves do not establish native federation lineage.
			}
			for _, key := range []string{"from_project_uid", "to_project_uid"} {
				endpoint, _ := StringValue(payload[key])
				if endpoint == project.UID {
					return ErrFederationProjectIdentityRequired
				}
			}
		}
		if len(events) < limit {
			return nil
		}
	}
}
