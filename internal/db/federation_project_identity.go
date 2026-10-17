package db

import (
	"context"
)

// CheckFederationProjectIdentityRetirement protects only former identities
// still referenced by another joined project's portable issue history. Archive
// and leave retain the identity; purge and merge must wait for those streams to
// detach. Local history covered by an adoption baseline creates no dependency.
func CheckFederationProjectIdentityRetirement(ctx context.Context, tx federationHistoryQuery, project Project, snapshotExpression string) error {
	joinedIDs, joinedUIDs := map[int64]bool{}, map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.uid FROM projects p
 JOIN federation_bindings b ON b.project_id=p.id WHERE p.id<>$1`, project.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var uid string
		if err := rows.Scan(&id, &uid); err != nil {
			_ = rows.Close()
			return err
		}
		joinedIDs[id], joinedUIDs[uid] = true, true
	}
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(joinedIDs) == 0 {
		return nil
	}
	owners := map[string]int64{}
	cutoffs := map[string]int64{}
	var afterID int64
	for {
		const limit = 1000
		rows, err := tx.QueryContext(ctx, `SELECT e.id,e.issue_uid,e.project_id,e.type,e.payload,i.project_id FROM events e
 JOIN issues i ON i.uid=e.issue_uid
 WHERE (e.project_id=$1 OR e.type='issue.moved') AND e.id>$2
 ORDER BY e.id LIMIT $3`, project.ID, afterID, limit)
		if err != nil {
			return err
		}
		var events []Event
		for rows.Next() {
			var event Event
			var issueUID, payload string
			var ownerID int64
			if err := rows.Scan(&event.ID, &issueUID, &event.ProjectID, &event.Type, &payload, &ownerID); err != nil {
				_ = rows.Close()
				return err
			}
			event.IssueUID, event.Payload = new(issueUID), payload
			owners[issueUID] = ownerID
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
			payload := PayloadMap([]byte(event.Payload))
			from, _ := StringValue(payload["from_project_uid"])
			nativeMove := event.Type == "issue.moved" && payload["snapshot"] != nil
			// Match the ordinary native feed: current ownership, original
			// envelope, or an outbound move from a still-joined source/transit.
			exposed := joinedIDs[owners[*event.IssueUID]] || joinedIDs[event.ProjectID] || (nativeMove && joinedUIDs[from])
			if !exposed {
				continue
			}
			if event.ProjectID == project.ID {
				return ErrFederationProjectIdentityRequired
			}
			if !nativeMove {
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
