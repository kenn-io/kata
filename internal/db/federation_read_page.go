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
	// For the last, the page read below ends in a probe of every owned issue
	// once the gap since afterID is wider than one window. When feed already
	// cleared that gap up to a recent ID, checking the newer events one at a
	// time reads less, as long as they are no more than the project's issues
	// and nothing else already requires a page read. The remembered range ends
	// no later than this snapshot's newest event, so the check is left out of
	// the statement when the range ends within a window of afterID; PostgreSQL
	// spends index reads planning it even when it never runs.
	foreignAfter, foreignThrough, foreignKnown := feed.foreignChecked(afterID)
	foreignKnown = foreignKnown && foreignAfter-afterID > max(int64(limit), feed.knownIssues())
	// Both bounds stay bound so the statement has one parameter list.
	foreign := `(CAST($4 AS BIGINT)<0 AND CAST($5 AS BIGINT)<0)`
	if foreignKnown {
		// The ownership check looks each event's issue up by UID alone, so the
		// planner can only answer it through the unique UID index; a project
		// condition inside it lets PostgreSQL walk every owned issue instead.
		foreign = `CASE WHEN m.own OR m.moved_out OR m.max_id>$5 THEN FALSE ELSE (SELECT e.id FROM events e WHERE e.id>$4 AND e.id<=$5 AND e.project_id<>$1 AND e.type<>'sync.reset_required'
 AND (SELECT i.project_id FROM issues i WHERE i.uid=e.issue_uid)=$1 ORDER BY e.id LIMIT 1) IS NOT NULL END`
	}
	var maxID int64
	var own, movedOut, foreignFound bool
	//nolint:gosec // G202: fixed backend fragments only; values stay bound.
	err := tx.QueryRowContext(ctx, `SELECT m.max_id, m.own, m.moved_out, `+foreign+` FROM (SELECT COALESCE((SELECT MAX(id) FROM events),0) AS max_id,
 EXISTS(SELECT 1 FROM events e`+projectIndex+` WHERE e.project_id=$1 AND e.id>$2 AND e.type<>'sync.reset_required') AS own,
 EXISTS(SELECT 1 FROM events e`+movedIndex+` WHERE e.type='issue.moved' AND `+movedFrom+`=$3 AND e.id>$2) AS moved_out) m`,
		project.ID, afterID, project.UID, foreignAfter, foreignThrough).Scan(&maxID, &own, &movedOut, &foreignFound)
	if err != nil {
		return nil, fmt.Errorf("federation event presence: %w", err)
	}
	probed := foreignKnown && !own && !movedOut && maxID <= foreignThrough
	if maxID <= afterID || (probed && !foreignFound) {
		feed.recordQuiet(afterID, maxID)
		return []Event{}, nil
	}
	var issueCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE project_id=$1`, project.ID).Scan(&issueCount); err != nil {
		return nil, fmt.Errorf("federation issue count: %w", err)
	}
	feed.recordIssues(issueCount)
	width := max(int64(limit), issueCount)
	// Both forms select the same events. Looking up each event's issue costs
	// one index probe per event; reading the owned issues costs one row per
	// issue. Use whichever reads less for this window.
	byEvent := `(e.project_id=$1 OR (SELECT i.project_id FROM issues i WHERE i.uid=e.issue_uid)=$1
 OR (e.type='issue.moved' AND ` + movedFrom + `=(SELECT uid FROM projects WHERE id=$1))) AND e.type<>'sync.reset_required'`
	byIssue := `(e.project_id=$1 OR e.issue_uid IN (SELECT uid FROM issues WHERE project_id=$1)
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
		visible := byIssue
		if min(end, maxID)-pos < issueCount {
			visible = byEvent
		}
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
