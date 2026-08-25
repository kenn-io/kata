package pgstore

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// ParentShortIDsByIssues maps child row IDs to current parent display IDs.
func (s *Store) ParentShortIDsByIssues(ctx context.Context, issueIDs []int64) (map[int64]string, error) {
	parents := map[int64]string{}
	if len(issueIDs) == 0 {
		return parents, nil
	}
	rows, err := s.QueryContext(ctx, `SELECT l.from_issue_id, parent.short_id
		FROM links l
		JOIN issues parent ON parent.id = l.to_issue_id
		WHERE l.type = 'parent' AND l.from_issue_id = ANY($1::bigint[])
		ORDER BY l.from_issue_id ASC`, issueIDs)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var childID int64
		var shortID string
		if err := rows.Scan(&childID, &shortID); err != nil {
			return nil, mapSQLError(err, nil)
		}
		parents[childID] = shortID
	}
	return parents, mapSQLError(rows.Err(), nil)
}

// RelationshipsByIssues returns every link-derived fact for each requested
// issue. The per-kind predicates match the sqlite backend's exactly; only the
// placeholder style differs.
func (s *Store) RelationshipsByIssues(
	ctx context.Context, issueIDs []int64,
) (map[int64]db.IssueRelationships, error) {
	out := map[int64]db.IssueRelationships{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	if err := s.scanRelationshipPairs(ctx, `SELECT l.from_issue_id, l.to_issue_id
		FROM links l
		WHERE l.type = 'parent' AND l.from_issue_id = ANY($1::bigint[])
		ORDER BY l.from_issue_id ASC`, issueIDs, out,
		func(rel *db.IssueRelationships, peer int64) {
			value := peer
			rel.ParentIssueID = &value
		}); err != nil {
		return nil, err
	}
	if err := s.scanRelationshipPairs(ctx, `SELECT l.from_issue_id, blocked.id
		FROM links l
		JOIN issues blocked ON blocked.id = l.to_issue_id
		WHERE l.type = 'blocks'
		  AND blocked.deleted_at IS NULL
		  AND l.from_issue_id = ANY($1::bigint[])
		ORDER BY l.from_issue_id ASC, blocked.id ASC`, issueIDs, out,
		func(rel *db.IssueRelationships, peer int64) {
			rel.Blocks = append(rel.Blocks, peer)
		}); err != nil {
		return nil, err
	}
	if err := s.scanRelationshipPairs(ctx, `SELECT l.to_issue_id, blocker.id
		FROM links l
		JOIN issues blocker ON blocker.id = l.from_issue_id
		WHERE l.type = 'blocks'
		  AND blocker.deleted_at IS NULL
		  AND l.to_issue_id = ANY($1::bigint[])
		ORDER BY l.to_issue_id ASC, blocker.id ASC`, issueIDs, out,
		func(rel *db.IssueRelationships, peer int64) {
			rel.BlockedBy = append(rel.BlockedBy, peer)
		}); err != nil {
		return nil, err
	}
	if err := s.scanRelationshipPairs(ctx, `SELECT viewer_id, peer_id FROM (
		SELECT l.from_issue_id AS viewer_id, peer.id AS peer_id
		  FROM links l
		  JOIN issues peer ON peer.id = l.to_issue_id
		 WHERE l.type = 'related'
		   AND peer.deleted_at IS NULL
		   AND l.from_issue_id = ANY($1::bigint[])
		UNION ALL
		SELECT l.to_issue_id AS viewer_id, peer.id AS peer_id
		  FROM links l
		  JOIN issues peer ON peer.id = l.from_issue_id
		 WHERE l.type = 'related'
		   AND peer.deleted_at IS NULL
		   AND l.to_issue_id = ANY($1::bigint[])
	) relationships
	ORDER BY viewer_id ASC, peer_id ASC`, issueIDs, out,
		func(rel *db.IssueRelationships, peer int64) {
			rel.Related = append(rel.Related, peer)
		}); err != nil {
		return nil, err
	}
	if err := s.appendChildCountsToRelationships(ctx, issueIDs, out); err != nil {
		return nil, err
	}
	return out, s.appendActivelyBlockedToRelationships(ctx, issueIDs, out)
}

func (s *Store) scanRelationshipPairs(
	ctx context.Context,
	query string,
	issueIDs []int64,
	out map[int64]db.IssueRelationships,
	assign func(rel *db.IssueRelationships, peer int64),
) error {
	rows, err := s.QueryContext(ctx, query, issueIDs)
	if err != nil {
		return mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var viewerID, peerID int64
		if err := rows.Scan(&viewerID, &peerID); err != nil {
			return fmt.Errorf("scan relationship projection: %w", mapSQLError(err, nil))
		}
		rel := out[viewerID]
		assign(&rel, peerID)
		out[viewerID] = rel
	}
	return mapSQLError(rows.Err(), nil)
}

func (s *Store) appendChildCountsToRelationships(
	ctx context.Context, issueIDs []int64, out map[int64]db.IssueRelationships,
) error {
	rows, err := s.QueryContext(ctx, `SELECT l.to_issue_id,
		COUNT(*) FILTER (WHERE child.status = 'open'), COUNT(*)
		FROM links l
		JOIN issues child ON child.id = l.from_issue_id
		JOIN projects child_project ON child_project.id = child.project_id
		WHERE l.type = 'parent'
		  AND child.deleted_at IS NULL
		  AND child_project.deleted_at IS NULL
		  AND l.to_issue_id = ANY($1::bigint[])
		GROUP BY l.to_issue_id
		ORDER BY l.to_issue_id ASC`, issueIDs)
	if err != nil {
		return mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var parentID int64
		var value db.ChildCounts
		if err := rows.Scan(&parentID, &value.Open, &value.Total); err != nil {
			return mapSQLError(err, nil)
		}
		rel := out[parentID]
		rel.Children = value
		out[parentID] = rel
	}
	return mapSQLError(rows.Err(), nil)
}

func (s *Store) appendActivelyBlockedToRelationships(
	ctx context.Context, issueIDs []int64, out map[int64]db.IssueRelationships,
) error {
	rows, err := s.QueryContext(ctx, `SELECT DISTINCT l.to_issue_id
		FROM links l
		JOIN issues blocked ON blocked.id = l.to_issue_id
		JOIN issues blocker ON blocker.id = l.from_issue_id
		JOIN projects blocker_project ON blocker_project.id = blocker.project_id
		WHERE l.type = 'blocks'
		  AND blocked.status = 'open'
		  AND blocked.deleted_at IS NULL
		  AND blocker.status = 'open'
		  AND blocker.deleted_at IS NULL
		  AND blocker_project.deleted_at IS NULL
		  AND l.to_issue_id = ANY($1::bigint[])`, issueIDs)
	if err != nil {
		return mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var issueID int64
		if err := rows.Scan(&issueID); err != nil {
			return mapSQLError(err, nil)
		}
		rel := out[issueID]
		rel.ActivelyBlocked = true
		out[issueID] = rel
	}
	return mapSQLError(rows.Err(), nil)
}

// ChildrenOfIssue returns visible direct children in list order.
func (s *Store) ChildrenOfIssue(ctx context.Context, parentIssueID int64) ([]db.Issue, error) {
	rows, err := s.QueryContext(ctx, issueSelect+`
		JOIN links l ON l.from_issue_id = i.id
		WHERE l.type = 'parent'
		  AND l.to_issue_id = $1
		  AND i.deleted_at IS NULL
		  AND p.deleted_at IS NULL
		ORDER BY i.updated_at DESC, i.id DESC`, parentIssueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var children []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		children = append(children, issue)
	}
	return children, mapSQLError(rows.Err(), nil)
}

// OpenChildrenOf returns a bounded open-child page plus the full count.
func (s *Store) OpenChildrenOf(
	ctx context.Context,
	parentIssueID int64,
	limit int,
) ([]db.Issue, int, error) {
	var total int
	if err := s.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM links l
		JOIN issues child ON child.id = l.from_issue_id
		JOIN projects child_project ON child_project.id = child.project_id
		WHERE l.type = 'parent'
		  AND l.to_issue_id = $1
		  AND child.status = 'open'
		  AND child.deleted_at IS NULL
		  AND child_project.deleted_at IS NULL`, parentIssueID).Scan(&total); err != nil {
		return nil, 0, mapSQLError(err, nil)
	}
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := s.QueryContext(ctx, issueSelect+`
		JOIN links l ON l.from_issue_id = i.id
		WHERE l.type = 'parent'
		  AND l.to_issue_id = $1
		  AND i.status = 'open'
		  AND i.deleted_at IS NULL
		  AND p.deleted_at IS NULL
		ORDER BY i.created_at ASC, i.id ASC
		LIMIT $2`, parentIssueID, limit)
	if err != nil {
		return nil, 0, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var children []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, 0, err
		}
		children = append(children, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapSQLError(err, nil)
	}
	return children, total, nil
}
