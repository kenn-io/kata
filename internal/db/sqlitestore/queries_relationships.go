package sqlitestore

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// RelationshipsByIssues returns every link-derived fact for each requested
// issue. One chunking discipline replaces six; the per-kind statements keep
// their own WHERE clauses because those predicates deliberately differ (see
// db.IssueRelationships).
func (d *Store) RelationshipsByIssues(
	ctx context.Context, issueIDs []int64,
) (map[int64]db.IssueRelationships, error) {
	out := map[int64]db.IssueRelationships{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := min(i+relationshipChunkSize, len(issueIDs))
		if err := d.appendRelationshipsForChunk(ctx, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendRelationshipsForChunk(
	ctx context.Context, chunk []int64, out map[int64]db.IssueRelationships,
) error {
	placeholders, args := relationshipChunkPlaceholders(chunk)

	// Maps child issue id → parent issue row id; callers resolve the row id
	// to a display identity themselves (parents may live in other projects).
	if err := d.scanRelationshipPairs(ctx, `SELECT l.from_issue_id, parent.id
	          FROM links l
	          JOIN issues child ON child.id = l.from_issue_id
	          JOIN issues parent ON parent.id = l.to_issue_id
	          WHERE l.type = 'parent'
	            AND l.from_issue_id IN (`+placeholders+`)
	          ORDER BY l.from_issue_id ASC`, args, out,
		func(rel *db.IssueRelationships, peer int64) {
			value := peer
			rel.ParentIssueID = &value
		}); err != nil {
		return fmt.Errorf("parent numbers by issues: %w", err)
	}

	if err := d.scanRelationshipPairs(ctx, `SELECT l.from_issue_id, blocked.id
	          FROM links l
	          JOIN issues blocker ON blocker.id = l.from_issue_id
	          JOIN issues blocked ON blocked.id = l.to_issue_id
	          WHERE l.type = 'blocks'
	            AND blocked.deleted_at IS NULL
	            AND l.from_issue_id IN (`+placeholders+`)
	          ORDER BY l.from_issue_id ASC, blocked.id ASC`, args, out,
		func(rel *db.IssueRelationships, peer int64) {
			rel.Blocks = append(rel.Blocks, peer)
		}); err != nil {
		return fmt.Errorf("block numbers by issues: %w", err)
	}

	// Full relationship hydration: a blocker in another project — including
	// one whose project is archived — is still returned. Display policy is
	// the separate ActivelyBlocked pass below.
	if err := d.scanRelationshipPairs(ctx, `SELECT l.to_issue_id, blocker.id
	          FROM links l
	          JOIN issues blocker ON blocker.id = l.from_issue_id
	          JOIN issues blocked ON blocked.id = l.to_issue_id
	          WHERE l.type = 'blocks'
	            AND blocker.deleted_at IS NULL
	            AND l.to_issue_id IN (`+placeholders+`)
	          ORDER BY l.to_issue_id ASC, blocker.id ASC`, args, out,
		func(rel *db.IssueRelationships, peer int64) {
			rel.BlockedBy = append(rel.BlockedBy, peer)
		}); err != nil {
		return fmt.Errorf("blocked-by numbers by issues: %w", err)
	}

	// Project both directions so a viewer on either canonical end sees
	// the other endpoint. Live-only join on the peer side mirrors what
	// the blocks queries do for soft-delete tolerance.
	related := `SELECT viewer_id, peer_number FROM (
	            SELECT l.from_issue_id AS viewer_id, peer.id AS peer_number
	              FROM links l
	              JOIN issues peer ON peer.id = l.to_issue_id
	             WHERE l.type = 'related'
	               AND peer.deleted_at IS NULL
	               AND l.from_issue_id IN (` + placeholders + `)
	            UNION ALL
	            SELECT l.to_issue_id AS viewer_id, peer.id AS peer_number
	              FROM links l
	              JOIN issues peer ON peer.id = l.from_issue_id
	             WHERE l.type = 'related'
	               AND peer.deleted_at IS NULL
	               AND l.to_issue_id IN (` + placeholders + `)
	          ) ORDER BY viewer_id ASC, peer_number ASC`
	// Each chunk's args are reused for both halves of the UNION.
	combined := make([]any, 0, len(args)*2)
	combined = append(combined, args...)
	combined = append(combined, args...)
	if err := d.scanRelationshipPairs(ctx, related, combined, out,
		func(rel *db.IssueRelationships, peer int64) {
			rel.Related = append(rel.Related, peer)
		}); err != nil {
		return fmt.Errorf("related numbers by issues: %w", err)
	}

	if err := d.appendChildCountsToRelationships(ctx, placeholders, args, out); err != nil {
		return err
	}
	return d.appendActivelyBlockedToRelationships(ctx, placeholders, args, out)
}

func (d *Store) scanRelationshipPairs(
	ctx context.Context,
	query string,
	args []any,
	out map[int64]db.IssueRelationships,
	assign func(rel *db.IssueRelationships, peer int64),
) error {
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var viewerID, peerID int64
		if err := rows.Scan(&viewerID, &peerID); err != nil {
			return err
		}
		rel := out[viewerID]
		assign(&rel, peerID)
		out[viewerID] = rel
	}
	return rows.Err()
}

func (d *Store) appendChildCountsToRelationships(
	ctx context.Context, placeholders string, args []any, out map[int64]db.IssueRelationships,
) error {
	query := `SELECT l.to_issue_id,
	                 SUM(CASE WHEN child.status = 'open' THEN 1 ELSE 0 END) AS open_count,
	                 COUNT(*) AS total_count
	          FROM links l
	          JOIN issues child ON child.id = l.from_issue_id
	          JOIN projects cp ON cp.id = child.project_id
	          WHERE l.type = 'parent'
	            AND child.deleted_at IS NULL
	            AND cp.deleted_at IS NULL
	            AND l.to_issue_id IN (` + placeholders + `)
	          GROUP BY l.to_issue_id
	          ORDER BY l.to_issue_id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("child counts by parents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var parentID int64
		var counts db.ChildCounts
		if err := rows.Scan(&parentID, &counts.Open, &counts.Total); err != nil {
			return fmt.Errorf("scan child counts by parents: %w", err)
		}
		rel := out[parentID]
		rel.Children = counts
		out[parentID] = rel
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate child counts by parents: %w", err)
	}
	return nil
}

func (d *Store) appendActivelyBlockedToRelationships(
	ctx context.Context, placeholders string, args []any, out map[int64]db.IssueRelationships,
) error {
	// Mirrors the ReadyIssues predicate on both sides: the target row must be
	// an open, live issue, and an incoming `blocks` link must carry an open,
	// live blocker in a non-archived project. DISTINCT collapses multiple
	// qualifying blockers to one row.
	query := `SELECT DISTINCT l.to_issue_id
	          FROM links l
	          JOIN issues blocked ON blocked.id = l.to_issue_id
	          JOIN issues blocker ON blocker.id = l.from_issue_id
	          JOIN projects bp ON bp.id = blocker.project_id
	          WHERE l.type = 'blocks'
	            AND blocked.status = 'open'
	            AND blocked.deleted_at IS NULL
	            AND blocker.status = 'open'
	            AND blocker.deleted_at IS NULL
	            AND bp.deleted_at IS NULL
	            AND l.to_issue_id IN (` + placeholders + `)`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("actively blocked issue ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blockedID int64
		if err := rows.Scan(&blockedID); err != nil {
			return fmt.Errorf("scan actively blocked issue ids: %w", err)
		}
		rel := out[blockedID]
		rel.ActivelyBlocked = true
		out[blockedID] = rel
	}
	return rows.Err()
}
