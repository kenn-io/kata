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
// feed, when set, lets a quiet project's poll skip probing every issue it owns.
func ReadFederationEventPage(ctx context.Context, tx *sql.Tx, project Project, issueUID string, afterID int64, limit int, postgres bool, feed *FederationFeedRead, query func(string, ...any) ([]Event, error)) ([]Event, error) {
	if issueUID != "" {
		return query(` WHERE e.issue_uid=$1 AND e.type<>'sync.reset_required' AND e.id>$2 ORDER BY e.id LIMIT $3`, issueUID, afterID, limit)
	}
	movedFrom := `json_extract(e.payload,'$.from_project_uid')`
	projectIndex, movedIndex, identityIndex, subjectUID := "", "", "", "subject.uid"
	if postgres {
		movedFrom = `e.payload::jsonb->>'from_project_uid'`
	} else {
		// ANALYZE can otherwise favor an ID walk through unrelated history.
		projectIndex, movedIndex, identityIndex = " INDEXED BY idx_events_project", " INDEXED BY idx_events_moved_from_id", " INDEXED BY idx_events_issue_uid_id"
		subjectUID = "+subject.uid"
	}
	// A feed event is stamped to the project, moves an issue out of it, or is
	// stamped elsewhere about an issue it owns. The first two are index seeks.
	// The last needs a probe per owned issue unless feed already cleared every
	// ID up to some point, leaving only newer events to check.
	foreignAfter, foreignKnown := feed.foreignChecked(afterID)
	foreign := `CAST($4 AS BIGINT)<0`
	if foreignKnown {
		// OFFSET keeps PostgreSQL from turning the ownership check into a
		// join that walks every owned issue; each newer event is checked alone.
		foreign = `(SELECT e.id FROM events e WHERE e.id>$4 AND e.project_id<>$1 AND e.type<>'sync.reset_required'
 AND EXISTS(SELECT 1 FROM issues i WHERE i.uid=e.issue_uid AND i.project_id=$1 LIMIT 1 OFFSET 0) ORDER BY e.id LIMIT 1) IS NOT NULL`
	}
	var maxID int64
	var own, movedOut, foreignFound bool
	//nolint:gosec // G202: fixed backend fragments only; values stay bound.
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT MAX(id) FROM events),0),
 EXISTS(SELECT 1 FROM events e`+projectIndex+` WHERE e.project_id=$1 AND e.id>$2 AND e.type<>'sync.reset_required'),
 EXISTS(SELECT 1 FROM events e`+movedIndex+` WHERE e.type='issue.moved' AND `+movedFrom+`=$3 AND e.id>$2),
 `+foreign, project.ID, afterID, project.UID, foreignAfter).Scan(&maxID, &own, &movedOut, &foreignFound)
	if err != nil {
		return nil, fmt.Errorf("federation event presence: %w", err)
	}
	if maxID <= afterID || (foreignKnown && !own && !movedOut && !foreignFound) {
		feed.recordQuiet(afterID, maxID)
		return []Event{}, nil
	}
	var issueCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE project_id=$1`, project.ID).Scan(&issueCount); err != nil {
		return nil, fmt.Errorf("federation issue count: %w", err)
	}
	width := max(int64(limit), issueCount)
	visible := `(e.project_id=$1 OR e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)
 OR (e.type='issue.moved' AND ` + movedFrom + `=(SELECT uid FROM projects WHERE id=$1))) AND e.type<>'sync.reset_required'`
	next := ` WHERE e.id=(SELECT MIN(id) FROM (
 SELECT (SELECT e.id FROM events e` + projectIndex + ` WHERE e.project_id=$1 AND e.id>$2 AND e.type<>'sync.reset_required' ORDER BY e.id LIMIT 1) AS id
 UNION ALL SELECT (SELECT e.id FROM events e` + movedIndex + ` WHERE e.type='issue.moved' AND ` + movedFrom + `=$3 AND e.id>$2 ORDER BY e.id LIMIT 1)
 UNION ALL SELECT (SELECT e.id FROM events e` + identityIndex + ` WHERE e.issue_uid=` + subjectUID + ` AND e.id>$2 AND e.type<>'sync.reset_required' ORDER BY e.id LIMIT 1)
 FROM issues subject WHERE subject.project_id=$1
 ) candidates)`
	events := []Event{}
	// Nothing in this snapshot is newer than maxID.
	for pos, span := afterID, width; len(events) < limit && pos < maxID; {
		end := pos + min(span, math.MaxInt64-pos)
		page, err := query(` WHERE `+visible+` AND e.id>$2 AND e.id<=$3 ORDER BY e.id LIMIT $4`, project.ID, pos, end, limit-len(events))
		if err != nil {
			return nil, err
		}
		// Widen after each productive window and reset after each seek.
		events, pos, span = append(events, page...), end, span+min(span, math.MaxInt64-span)
		if len(page) == 0 && pos < maxID {
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
	if len(events) == 0 {
		feed.recordQuiet(afterID, maxID)
	}
	return events, nil
}
