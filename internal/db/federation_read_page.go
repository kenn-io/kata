package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// ReadFederationEventPage skips unrelated gaps within the caller's snapshot.
// query appends a suffix to the backend event projection and decodes its rows.
// ReadFederationTransaction validates issue ownership before reading pages.
func ReadFederationEventPage(ctx context.Context, tx *sql.Tx, project Project, issueUID string, afterID int64, limit int, postgres bool, query func(string, ...any) ([]Event, error)) ([]Event, error) {
	identity := FederationReadIssueUIDExpr(postgres)
	if issueUID != "" {
		return query(` WHERE `+identity+`=$1 AND e.type<>'sync.reset_required' AND e.id>$2 ORDER BY e.id LIMIT $3`, issueUID, afterID, limit)
	}
	var hasEvents bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE id>$1)`, afterID).Scan(&hasEvents); err != nil {
		return nil, fmt.Errorf("federation event presence: %w", err)
	}
	if !hasEvents {
		return []Event{}, nil
	}
	movedFrom := `json_extract(e.payload,'$.from_project_uid')`
	projectIndex, movedIndex, identityIndex, subjectUID := "", "", "", "subject.uid"
	if postgres {
		movedFrom = `e.payload::jsonb->>'from_project_uid'`
	} else {
		// ANALYZE can otherwise favor an ID walk through unrelated history.
		projectIndex, movedIndex, identityIndex = " INDEXED BY idx_events_project", " INDEXED BY idx_events_moved_from_id", " INDEXED BY idx_events_resolved_issue_uid"
		subjectUID = "+subject.uid"
	}
	var issueCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE project_id=$1`, project.ID).Scan(&issueCount); err != nil {
		return nil, fmt.Errorf("federation issue count: %w", err)
	}
	width := max(int64(limit), issueCount)
	visible := `(e.project_id=$1 OR ` + identity + ` IN (SELECT uid FROM issues WHERE project_id=$1)
 OR (e.type='issue.moved' AND ` + movedFrom + `=(SELECT uid FROM projects WHERE id=$1))) AND e.type<>'sync.reset_required'`
	next := ` WHERE e.id=(SELECT MIN(id) FROM (
 SELECT (SELECT e.id FROM events e` + projectIndex + ` WHERE e.project_id=$1 AND e.id>$2 AND e.type<>'sync.reset_required' ORDER BY e.id LIMIT 1) AS id
 UNION ALL SELECT (SELECT e.id FROM events e` + movedIndex + ` WHERE e.type='issue.moved' AND ` + movedFrom + `=$3 AND e.id>$2 ORDER BY e.id LIMIT 1)
 UNION ALL SELECT (SELECT e.id FROM events e` + identityIndex + ` WHERE ` + identity + `=` + subjectUID + ` AND e.id>$2 AND e.type<>'sync.reset_required' ORDER BY e.id LIMIT 1)
 FROM issues subject WHERE subject.project_id=$1
 ) candidates)`
	events := []Event{}
	for pos, span := afterID, width; len(events) < limit && pos < math.MaxInt64; {
		end := pos + min(span, math.MaxInt64-pos)
		page, err := query(` WHERE `+visible+` AND e.id>$2 AND e.id<=$3 ORDER BY e.id LIMIT $4`, project.ID, pos, end, limit-len(events))
		if err != nil {
			return nil, err
		}
		// Widen after each productive window and reset after each seek.
		events, pos, span = append(events, page...), end, span+min(span, math.MaxInt64-span)
		if len(page) == 0 {
			candidate, err := query(next, project.ID, pos, project.UID)
			if err != nil {
				return nil, err
			}
			if len(candidate) == 0 {
				break
			}
			pos, span = candidate[0].ID-1, width
		}
	}
	return events, nil
}
