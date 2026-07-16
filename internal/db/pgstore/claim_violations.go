package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

const claimViolationSelect = `SELECT e.id,e.uid,
COALESCE(e.issue_uid,e.payload::jsonb->>'issue_uid',''),COALESCE(i.short_id,''),
COALESCE(e.payload::jsonb->>'offending_event_uid',e.payload::jsonb->>'event_uid',''),
COALESCE(e.payload::jsonb->>'offending_event_type',e.payload::jsonb->>'event_type',''),
COALESCE(e.payload::jsonb->>'offending_origin_instance_uid',e.payload::jsonb->>'origin_instance_uid',''),
COALESCE(e.payload::jsonb->>'actor',e.actor,''),COALESCE(e.payload::jsonb->>'reason',''),e.created_at
FROM events e LEFT JOIN issues i ON i.project_id=e.project_id AND i.uid=e.issue_uid`

// UnresolvedClaimViolationsForIssue returns one issue's violations after its latest release boundary.
func (s *Store) UnresolvedClaimViolationsForIssue(
	ctx context.Context,
	projectID int64,
	issueUID string,
	limit int,
) ([]db.ClaimViolationSummary, int64, error) {
	if limit < 0 {
		limit = 0
	}
	cutoff, err := claimViolationCutoffForIssue(ctx, s.DB, projectID, issueUID)
	if err != nil {
		return nil, 0, err
	}
	count, err := countUnresolvedClaimViolationsForIssue(ctx, s.DB, projectID, issueUID, cutoff)
	if err != nil {
		return nil, 0, err
	}
	if limit == 0 {
		return []db.ClaimViolationSummary{}, count, nil
	}
	rows, err := s.QueryContext(ctx, claimViolationSelect+`
WHERE e.project_id=$1 AND e.issue_uid=$2 AND e.type='claim.violated' AND e.id>$3
ORDER BY e.id DESC LIMIT $4`, projectID, issueUID, cutoff, limit)
	if err != nil {
		return nil, 0, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	violations, err := scanClaimViolationSummaries(rows)
	return violations, count, err
}

// UnresolvedClaimViolationsForProject returns recent unresolved violations across one project.
func (s *Store) UnresolvedClaimViolationsForProject(
	ctx context.Context,
	projectID int64,
	limit int,
) ([]db.ClaimViolationSummary, int64, error) {
	if limit < 0 {
		limit = 0
	}
	count, err := countUnresolvedClaimViolationsForProject(ctx, s.DB, projectID)
	if err != nil {
		return nil, 0, err
	}
	if limit == 0 {
		return []db.ClaimViolationSummary{}, count, nil
	}
	rows, err := s.QueryContext(ctx, claimViolationSelect+`
WHERE e.project_id=$1 AND e.type='claim.violated' AND e.id>COALESCE(
 (SELECT MAX(r.id) FROM events r WHERE r.project_id=e.project_id AND r.issue_uid=e.issue_uid
  AND r.type IN ('claim.released','claim.expired','claim.force_released')),
 (SELECT MIN(a.id) FROM events a WHERE a.project_id=e.project_id AND a.issue_uid=e.issue_uid
  AND a.type='claim.acquired'),9223372036854775807)
ORDER BY e.id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, 0, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	violations, err := scanClaimViolationSummaries(rows)
	return violations, count, err
}

type claimViolationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func claimViolationCutoffForIssue(
	ctx context.Context,
	queryer claimViolationQueryer,
	projectID int64,
	issueUID string,
) (int64, error) {
	var cutoff int64
	err := queryer.QueryRowContext(ctx, `SELECT COALESCE(
 (SELECT MAX(id) FROM events WHERE project_id=$1 AND issue_uid=$2
  AND type IN ('claim.released','claim.expired','claim.force_released')),
 (SELECT MIN(id) FROM events WHERE project_id=$1 AND issue_uid=$2 AND type='claim.acquired'),
 9223372036854775807)`, projectID, issueUID).Scan(&cutoff)
	return cutoff, mapSQLError(err, nil)
}

func countUnresolvedClaimViolationsForIssue(
	ctx context.Context,
	queryer claimViolationQueryer,
	projectID int64,
	issueUID string,
	cutoff int64,
) (int64, error) {
	var count int64
	err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM events
WHERE project_id=$1 AND issue_uid=$2 AND type='claim.violated' AND id>$3`,
		projectID, issueUID, cutoff).Scan(&count)
	return count, mapSQLError(err, nil)
}

func countUnresolvedClaimViolationsForProject(
	ctx context.Context,
	queryer claimViolationQueryer,
	projectID int64,
) (int64, error) {
	var count int64
	err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM events e
WHERE e.project_id=$1 AND e.type='claim.violated' AND e.id>COALESCE(
 (SELECT MAX(r.id) FROM events r WHERE r.project_id=e.project_id AND r.issue_uid=e.issue_uid
  AND r.type IN ('claim.released','claim.expired','claim.force_released')),
 (SELECT MIN(a.id) FROM events a WHERE a.project_id=e.project_id AND a.issue_uid=e.issue_uid
  AND a.type='claim.acquired'),9223372036854775807)`, projectID).Scan(&count)
	return count, mapSQLError(err, nil)
}

func scanClaimViolationSummaries(rows *sql.Rows) ([]db.ClaimViolationSummary, error) {
	output := []db.ClaimViolationSummary{}
	for rows.Next() {
		var violation db.ClaimViolationSummary
		var recordedAt string
		if err := rows.Scan(&violation.EventID, &violation.EventUID, &violation.IssueUID,
			&violation.IssueShortID, &violation.OffendingEventUID, &violation.OffendingEventType,
			&violation.OffendingOriginInstanceUID, &violation.Actor, &violation.Reason,
			&recordedAt); err != nil {
			return nil, mapSQLError(err, nil)
		}
		parsed, err := parseStoredTime(recordedAt)
		if err != nil {
			return nil, err
		}
		violation.At = parsed
		output = append(output, violation)
	}
	return output, mapSQLError(rows.Err(), nil)
}
