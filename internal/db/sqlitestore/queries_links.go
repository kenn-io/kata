package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// CreateLink inserts a links row. Distinct error types let the caller emit
// the right wire status without parsing SQLite messages.
func (d *Store) CreateLink(ctx context.Context, p db.CreateLinkParams) (db.Link, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
		p.ProjectID, p.FromIssueID, p.ToIssueID, p.FromIssueID, p.ToIssueID, p.Type, p.Author)
	if err != nil {
		classified := classifyLinkInsertError(err)
		// SQLite may report the partial-parent index violation as a bare
		// `links.from_issue_id` UNIQUE failure, which classifies to
		// ErrParentAlreadySet. For an exact-duplicate parent link the
		// caller-facing semantic is "already linked" (200 no-op), not
		// "different parent set" (409 conflict). Disambiguate by re-querying.
		if errors.Is(classified, db.ErrParentAlreadySet) && p.Type == "parent" {
			if _, lookupErr := d.LinkByEndpoints(ctx, p.FromIssueID, p.ToIssueID, "parent"); lookupErr == nil {
				return db.Link{}, db.ErrLinkExists
			}
		}
		return db.Link{}, classified
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.Link{}, fmt.Errorf("last insert id: %w", err)
	}
	return d.LinkByID(ctx, id)
}

// classifyLinkInsertError maps SQLite constraint failures to typed errors so
// the handler can choose the right HTTP status without string-matching.
//
// Order matters: the triple-UNIQUE check must run before the partial-parent
// check because both messages start with "links.from_issue_id". The triple is
// distinguishable by the trailing column list; once that case is rejected,
// any remaining "links.from_issue_id" UNIQUE error must be the partial index
// on (from_issue_id) WHERE type='parent'. modernc.org/sqlite's error text for
// partial-index violations names only the indexed column, not the WHERE
// clause — see TestCreateLink_SecondParentIsErrParentAlreadySet.
func classifyLinkInsertError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: links.from_issue_id, links.to_issue_id, links.type"):
		return db.ErrLinkExists
	case strings.Contains(msg, "UNIQUE constraint failed: links.from_issue_id"):
		return db.ErrParentAlreadySet
	case strings.Contains(msg, "CHECK constraint failed") &&
		strings.Contains(msg, "from_issue_id <> to_issue_id"):
		return db.ErrSelfLink
	case strings.Contains(msg, "cross-project links are not allowed"):
		return db.ErrCrossProjectLink
	}
	return fmt.Errorf("insert link: %w", err)
}

// LinkByID fetches a link by rowid.
func (d *Store) LinkByID(ctx context.Context, id int64) (db.Link, error) {
	row := d.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, id)
	return scanLink(row)
}

// LinkByEndpoints fetches the link for a (from, to, type) triple.
func (d *Store) LinkByEndpoints(ctx context.Context, fromIssueID, toIssueID int64, linkType string) (db.Link, error) {
	row := d.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
		fromIssueID, toIssueID, linkType)
	return scanLink(row)
}

// ParentOf returns the parent link for childIssueID (one-parent invariant).
// Returns ErrNotFound when no parent is set.
func (d *Store) ParentOf(ctx context.Context, childIssueID int64) (db.Link, error) {
	row := d.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND type = 'parent'`,
		childIssueID)
	return scanLink(row)
}

const relationshipChunkSize = labelsByIssuesChunkSize

// ParentShortIDsByIssues returns child issue ID -> parent short_id for
// parent links inside projectID. Used by the audit handler to render and
// filter close rows by parent ref.
func (d *Store) ParentShortIDsByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64]string, error) {
	out := map[int64]string{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		placeholders, args := relationshipChunkPlaceholders(projectID, issueIDs[i:end])
		query := `SELECT l.from_issue_id, parent.short_id
		          FROM links l
		          JOIN issues child  ON child.id  = l.from_issue_id
		          JOIN issues parent ON parent.id = l.to_issue_id
		          WHERE l.project_id = ?
		            AND child.project_id = ?
		            AND parent.project_id = ?
		            AND l.type = 'parent'
		            AND l.from_issue_id IN (` + placeholders + `)`
		rows, err := d.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("parent short ids by issues: %w", err)
		}
		if err := scanParentShortIDs(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanParentShortIDs(rows *sql.Rows, out map[int64]string) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var childID int64
		var parentShortID string
		if err := rows.Scan(&childID, &parentShortID); err != nil {
			return fmt.Errorf("scan parent short id: %w", err)
		}
		out[childID] = parentShortID
	}
	return rows.Err()
}

// ParentNumbersByIssues returns child issue ID -> parent issue id for
// parent links inside projectID. Despite the name (transitional), the map
// value is the parent's rowid, not a user-facing number; downstream code
// resolves it to a LinkPeer.
func (d *Store) ParentNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendParentNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendParentNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	// Result references the parent issue's id while callers transition off
	// of `number` (Tasks 11/13/14). The map shape is preserved so downstream
	// list/show code keeps compiling against int64 keys until it migrates.
	query := `SELECT l.from_issue_id, parent.id
	          FROM links l
	          JOIN issues child ON child.id = l.from_issue_id
	          JOIN issues parent ON parent.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND child.project_id = ?
	            AND parent.project_id = ?
	            AND l.type = 'parent'
	            AND l.from_issue_id IN (` + placeholders + `)
	          ORDER BY l.from_issue_id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("parent numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var childID, parentNumber int64
		if err := rows.Scan(&childID, &parentNumber); err != nil {
			return fmt.Errorf("scan parent numbers by issues: %w", err)
		}
		out[childID] = parentNumber
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate parent numbers by issues: %w", err)
	}
	return nil
}

// BlockNumbersByIssues returns issue ID -> issue numbers directly blocked by
// that issue for outgoing "blocks" links inside projectID.
func (d *Store) BlockNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendBlockNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendBlockNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	query := `SELECT l.from_issue_id, blocked.id
	          FROM links l
	          JOIN issues blocker ON blocker.id = l.from_issue_id
	          JOIN issues blocked ON blocked.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND blocker.project_id = ?
	            AND blocked.project_id = ?
	            AND l.type = 'blocks'
	            AND blocked.deleted_at IS NULL
	            AND l.from_issue_id IN (` + placeholders + `)
	          ORDER BY l.from_issue_id ASC, blocked.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("block numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blockerID, blockedNumber int64
		if err := rows.Scan(&blockerID, &blockedNumber); err != nil {
			return fmt.Errorf("scan block numbers by issues: %w", err)
		}
		out[blockerID] = append(out[blockerID], blockedNumber)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate block numbers by issues: %w", err)
	}
	return nil
}

// BlockedByNumbersByIssues returns issue ID -> issue numbers that block
// that issue. Inverse of BlockNumbersByIssues: for each issue X, the
// returned numbers are the issues whose outgoing `blocks` link points
// at X. Used by `kata list --json` to surface every relationship type
// per row, not just outgoing blocks.
func (d *Store) BlockedByNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendBlockedByNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendBlockedByNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	query := `SELECT l.to_issue_id, blocker.id
	          FROM links l
	          JOIN issues blocker ON blocker.id = l.from_issue_id
	          JOIN issues blocked ON blocked.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND blocker.project_id = ?
	            AND blocked.project_id = ?
	            AND l.type = 'blocks'
	            AND blocker.deleted_at IS NULL
	            AND l.to_issue_id IN (` + placeholders + `)
	          ORDER BY l.to_issue_id ASC, blocker.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("blocked-by numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blockedID, blockerNumber int64
		if err := rows.Scan(&blockedID, &blockerNumber); err != nil {
			return fmt.Errorf("scan blocked-by numbers by issues: %w", err)
		}
		out[blockedID] = append(out[blockedID], blockerNumber)
	}
	return rows.Err()
}

// RelatedNumbersByIssues returns issue ID -> issue numbers symmetrically
// related to that issue. Related links are stored canonically as (from <
// to), so for any viewer X the peers may sit on either side; the query
// projects both directions.
func (d *Store) RelatedNumbersByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendRelatedNumbersForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Store) appendRelatedNumbersForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]int64,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	// Project both directions so a viewer on either canonical end sees
	// the other endpoint. Live-only join on the peer side mirrors what
	// the blocks queries do for soft-delete tolerance.
	query := `SELECT viewer_id, peer_number FROM (
	            SELECT l.from_issue_id AS viewer_id, peer.id AS peer_number
	              FROM links l
	              JOIN issues self ON self.id = l.from_issue_id
	              JOIN issues peer ON peer.id = l.to_issue_id
	             WHERE l.project_id = ?
	               AND self.project_id = ?
	               AND peer.project_id = ?
	               AND l.type = 'related'
	               AND peer.deleted_at IS NULL
	               AND l.from_issue_id IN (` + placeholders + `)
	            UNION ALL
	            SELECT l.to_issue_id AS viewer_id, peer.id AS peer_number
	              FROM links l
	              JOIN issues self ON self.id = l.to_issue_id
	              JOIN issues peer ON peer.id = l.from_issue_id
	             WHERE l.project_id = ?
	               AND self.project_id = ?
	               AND peer.project_id = ?
	               AND l.type = 'related'
	               AND peer.deleted_at IS NULL
	               AND l.to_issue_id IN (` + placeholders + `)
	          ) ORDER BY viewer_id ASC, peer_number ASC`
	// Each chunk's args are reused for both halves of the UNION.
	combined := make([]any, 0, len(args)*2)
	combined = append(combined, args...)
	combined = append(combined, args...)
	rows, err := d.QueryContext(ctx, query, combined...)
	if err != nil {
		return fmt.Errorf("related numbers by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var viewerID, peerNumber int64
		if err := rows.Scan(&viewerID, &peerNumber); err != nil {
			return fmt.Errorf("scan related numbers by issues: %w", err)
		}
		out[viewerID] = append(out[viewerID], peerNumber)
	}
	return rows.Err()
}

// ChildCountsByParents returns direct-child open/total counts keyed by parent
// issue ID inside projectID.
func (d *Store) ChildCountsByParents(
	ctx context.Context, projectID int64, parentIssueIDs []int64,
) (map[int64]db.ChildCounts, error) {
	out := map[int64]db.ChildCounts{}
	if len(parentIssueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(parentIssueIDs); i += relationshipChunkSize {
		end := i + relationshipChunkSize
		if end > len(parentIssueIDs) {
			end = len(parentIssueIDs)
		}
		if err := d.appendChildCountsForChunk(ctx, projectID, parentIssueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// txHasOpenChildren reports whether parentIssueID has any non-deleted,
// non-closed children when run inside the close transaction. The daemon
// handler runs the user-friendly OpenChildrenOf-backed check first; this
// closes the race between that read and the close write by re-checking
// inside the same write transaction.
func txHasOpenChildren(ctx context.Context, tx *sql.Tx, projectID, parentIssueID int64) (bool, error) {
	var total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM links l
		 JOIN issues child ON child.id = l.from_issue_id
		 WHERE l.project_id = ?
		   AND child.project_id = ?
		   AND l.type = 'parent'
		   AND l.to_issue_id = ?
		   AND child.status = 'open'
		   AND child.deleted_at IS NULL`,
		projectID, projectID, parentIssueID).Scan(&total); err != nil {
		return false, fmt.Errorf("open children check: %w", err)
	}
	return total > 0, nil
}

// txParentIdentity returns the parent UID and short_id for childIssueID
// at the moment of the close transaction. UID is the stable identity
// (immutable across project merges and federation reshuffles); short_id
// is the close-time display value. ok=false signals "no parent set" so
// CloseIssue can write empty markers into the payload rather than
// dropping the fields entirely (the audit projection uses field
// presence to distinguish "no parent at close" from "legacy event that
// predates this freezing").
func txParentIdentity(ctx context.Context, tx *sql.Tx, childIssueID int64) (uid, shortID string, ok bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT parent.uid, parent.short_id
		 FROM links l
		 JOIN issues parent ON parent.id = l.to_issue_id
		 WHERE l.from_issue_id = ? AND l.type = 'parent'`,
		childIssueID).Scan(&uid, &shortID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("close-time parent lookup: %w", err)
	}
	return uid, shortID, true, nil
}

// OpenChildrenOf returns up to limit non-deleted, non-closed children of
// parentIssueID, plus the total open-children count. Used by the parent-
// close completeness check: the truncated slice feeds the error listing,
// and the full count drives the "(N more)" suffix.
func (d *Store) OpenChildrenOf(
	ctx context.Context, projectID, parentIssueID int64, limit int,
) ([]db.Issue, int, error) {
	var total int
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM links l
		 JOIN issues child ON child.id = l.from_issue_id
		 WHERE l.project_id = ?
		   AND child.project_id = ?
		   AND l.type = 'parent'
		   AND l.to_issue_id = ?
		   AND child.status = 'open'
		   AND child.deleted_at IS NULL`,
		projectID, projectID, parentIssueID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("open children count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := d.QueryContext(ctx, issueSelect+`
		JOIN links l ON l.from_issue_id = i.id
		WHERE l.project_id = ?
		  AND i.project_id = ?
		  AND l.type = 'parent'
		  AND l.to_issue_id = ?
		  AND i.status = 'open'
		  AND i.deleted_at IS NULL
		ORDER BY i.created_at ASC
		LIMIT ?`,
		projectID, projectID, parentIssueID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("open children: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate open children: %w", err)
	}
	return out, total, nil
}

// ChildrenOfIssue returns direct, non-deleted children for parentIssueID in
// the same order as ListIssues.
func (d *Store) ChildrenOfIssue(ctx context.Context, projectID, parentIssueID int64) ([]db.Issue, error) {
	query := issueSelect + `
		JOIN links l ON l.from_issue_id = i.id
		JOIN issues parent ON parent.id = l.to_issue_id
		WHERE l.project_id = ?
		  AND i.project_id = ?
		  AND parent.project_id = ?
		  AND l.type = 'parent'
		  AND l.to_issue_id = ?
		  AND i.deleted_at IS NULL
		ORDER BY i.updated_at DESC, i.id DESC`
	rows, err := d.QueryContext(ctx, query, projectID, projectID, projectID, parentIssueID)
	if err != nil {
		return nil, fmt.Errorf("children of issue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate children of issue: %w", err)
	}
	return out, nil
}

func (d *Store) appendChildCountsForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64]db.ChildCounts,
) error {
	placeholders, args := relationshipChunkPlaceholders(projectID, chunk)
	query := `SELECT l.to_issue_id,
	                 SUM(CASE WHEN child.status = 'open' THEN 1 ELSE 0 END) AS open_count,
	                 COUNT(*) AS total_count
	          FROM links l
	          JOIN issues child ON child.id = l.from_issue_id
	          JOIN issues parent ON parent.id = l.to_issue_id
	          WHERE l.project_id = ?
	            AND child.project_id = ?
	            AND parent.project_id = ?
	            AND l.type = 'parent'
	            AND child.deleted_at IS NULL
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
		out[parentID] = counts
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate child counts by parents: %w", err)
	}
	return nil
}

func relationshipChunkPlaceholders(projectID int64, chunk []int64) (string, []any) {
	placeholders := make([]string, len(chunk))
	args := make([]any, 0, len(chunk)+3)
	args = append(args, projectID, projectID, projectID)
	for i, id := range chunk {
		placeholders[i] = "?"
		args = append(args, id)
	}
	return strings.Join(placeholders, ","), args
}

// LinksByIssue returns every link involving issueID (either endpoint), ordered
// by id ASC. Used to build the show-issue response and to back the
// list-then-delete flow used by `kata edit --remove-*`.
func (d *Store) LinksByIssue(ctx context.Context, issueID int64) ([]db.Link, error) {
	rows, err := d.QueryContext(ctx,
		linkSelect+` WHERE from_issue_id = ? OR to_issue_id = ? ORDER BY id ASC`,
		issueID, issueID)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLinkByID removes a links row. Returns ErrNotFound when no row exists.
func (d *Store) DeleteLinkByID(ctx context.Context, linkID int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, linkID)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete link rows affected: %w", err)
	}
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

const linkSelect = `SELECT id, project_id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author, created_at FROM links`

func scanLink(r rowScanner) (db.Link, error) {
	var l db.Link
	err := r.Scan(&l.ID, &l.ProjectID, &l.FromIssueID, &l.FromIssueUID, &l.ToIssueID, &l.ToIssueUID, &l.Type, &l.Author, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Link{}, db.ErrNotFound
	}
	if err != nil {
		return db.Link{}, fmt.Errorf("scan link: %w", err)
	}
	return l, nil
}

// CreateLinkAndEvent inserts a link, emits the matching issue.linked event,
// and bumps the URL issue's updated_at — all in one TX. Returns the new link
// and the event row. Typed errors (ErrLinkExists, ErrParentAlreadySet,
// ErrSelfLink, ErrCrossProjectLink) flow up unchanged from the underlying
// INSERT classification.
//
// The DB-layer methods CreateLinkAndEvent and DeleteLinkAndEvent split "the
// link's storage endpoints" (from_issue_id/to_issue_id, possibly canonicalized
// for related) from "the issue the user acted on" (the URL ref, which
// determines events.issue_id and the updated_at bump).
//
// Used by the daemon's POST /links handler so the link insert and its event
// are atomic — there's no window where the row exists without an event.
//
// Storage endpoints come from p (canonicalized for related when fromID > toID
// at the call site); event attribution comes from ev. For parent/blocks the
// two coincide; for related they may differ when canonicalization swapped.
func (d *Store) CreateLinkAndEvent(ctx context.Context, p db.CreateLinkParams, ev db.LinkEventParams) (db.Link, db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, projectName, err := lookupIssueForEvent(ctx, tx, ev.EventIssueID)
	if err != nil {
		return db.Link{}, db.Event{}, err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
		p.ProjectID, p.FromIssueID, p.ToIssueID, p.FromIssueID, p.ToIssueID, p.Type, p.Author)
	if err != nil {
		classified := classifyLinkInsertError(err)
		// Same exact-duplicate-parent disambiguation as the non-TX CreateLink:
		// the partial-parent UNIQUE index produces the same error text whether
		// it's a different parent (409) or the exact same parent (200 no-op).
		// Re-query to tell them apart inside the same TX.
		if errors.Is(classified, db.ErrParentAlreadySet) && p.Type == "parent" {
			var n int
			qErr := tx.QueryRowContext(ctx,
				`SELECT 1 FROM links WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
				p.FromIssueID, p.ToIssueID, p.Type).Scan(&n)
			if qErr == nil {
				return db.Link{}, db.Event{}, db.ErrLinkExists
			}
		}
		return db.Link{}, db.Event{}, classified
	}
	linkID, err := res.LastInsertId()
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("last insert id: %w", err)
	}

	// related_issue_id is the OTHER endpoint of the link (not the URL issue).
	// When the URL issue is one of the link's endpoints, pick the opposite;
	// otherwise default to the link's to_issue_id.
	relatedID := p.ToIssueID
	if relatedID == ev.EventIssueID {
		relatedID = p.FromIssueID
	}
	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]any{
		"link_id":       linkID,
		"type":          p.Type,
		"from_short_id": ev.FromShortID,
		"from_uid":      ev.FromUID,
		"to_short_id":   ev.ToShortID,
		"to_uid":        ev.ToUID,
		"updated_at":    ts,
	})
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("marshal link payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:      p.ProjectID,
		ProjectName:    projectName,
		IssueID:        &ev.EventIssueID,
		RelatedIssueID: &relatedID,
		Type:           ev.EventType,
		Actor:          ev.Actor,
		Payload:        string(payload),
	})
	if err != nil {
		return db.Link{}, db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, ev.EventIssueID); err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("touch issue: %w", err)
	}

	// Re-fetch the inserted row INSIDE the TX so a post-commit failure
	// (context cancellation, concurrent deletion) can't leave the caller with
	// a 500 after the mutation has already committed.
	link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, linkID))
	if err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("re-fetch link inside tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return db.Link{}, db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return link, evt, nil
}

// DeleteLinkAndEvent deletes a link and emits the matching issue.unlinked
// event in one TX. The link to delete comes from the link argument; event
// attribution (events.issue_id, updated_at bump, payload
// from_short_id/to_short_id/uid) comes from ev. Returns ErrNotFound if the
// link is already gone — caller maps to 200 no-op envelope per spec §4.5.
func (d *Store) DeleteLinkAndEvent(ctx context.Context, link db.Link, ev db.LinkEventParams) (db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, projectName, err := lookupIssueForEvent(ctx, tx, ev.EventIssueID)
	if err != nil {
		return db.Event{}, err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, link.ID)
	if err != nil {
		return db.Event{}, fmt.Errorf("delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Event{}, fmt.Errorf("delete link rows affected: %w", err)
	}
	if n == 0 {
		return db.Event{}, db.ErrNotFound
	}
	relatedID := link.ToIssueID
	if relatedID == ev.EventIssueID {
		relatedID = link.FromIssueID
	}
	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]any{
		"link_id":       link.ID,
		"type":          link.Type,
		"from_short_id": ev.FromShortID,
		"from_uid":      ev.FromUID,
		"to_short_id":   ev.ToShortID,
		"to_uid":        ev.ToUID,
		"updated_at":    ts,
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal unlink payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:      link.ProjectID,
		ProjectName:    projectName,
		IssueID:        &ev.EventIssueID,
		RelatedIssueID: &relatedID,
		Type:           ev.EventType,
		Actor:          ev.Actor,
		Payload:        string(payload),
	})
	if err != nil {
		return db.Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, ev.EventIssueID); err != nil {
		return db.Event{}, fmt.Errorf("touch issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return evt, nil
}
