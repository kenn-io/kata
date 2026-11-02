package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/shortid"
)

// ErrIssueMoveDescendantArchived refuses a subtree whose descendant lives in an
// archived project. Restore the project or detach the descendant first.
var ErrIssueMoveDescendantArchived = errors.New("descendant is in an archived project")

// IssueMoveDescendant identifies the descendant that refused a subtree move.
type IssueMoveDescendant struct {
	UID         string `json:"uid"`
	ShortID     string `json:"short_id"`
	ProjectUID  string `json:"project_uid"`
	ProjectName string `json:"project_name"`
}

// IssueMoveDescendantError names the descendant whose move rule rejected the
// subtree. It unwraps to that rule's error.
type IssueMoveDescendantError struct {
	Descendant IssueMoveDescendant
	Err        error
}

func (e *IssueMoveDescendantError) Error() string {
	return fmt.Sprintf("descendant %s#%s: %v", e.Descendant.ProjectName, e.Descendant.ShortID, e.Err)
}

func (e *IssueMoveDescendantError) Unwrap() error { return e.Err }

// IssueMoveTreeItem is one issue in a move. Descendant is nil for the root.
type IssueMoveTreeItem struct {
	In         MoveIssueProjectIn
	Descendant *IssueMoveDescendant
}

// Refusal attributes err to this item when it is a descendant.
func (item IssueMoveTreeItem) Refusal(err error) error {
	if err == nil || item.Descendant == nil {
		return err
	}
	return &IssueMoveDescendantError{Descendant: *item.Descendant, Err: err}
}

// ErrIssueMoveTreeChanged reports membership changes since external validation.
var ErrIssueMoveTreeChanged = errors.New("issue move tree changed during validation")

// IssueMoveTreeInputs captures descendant revisions inside the move transaction.
// Both backends serialize the entire discovery/validation/write operation. Parent
// links point from child to parent and may cross project boundaries. A deleted
// descendant ends its branch, so its own subtree stays with it.
func IssueMoveTreeInputs(ctx context.Context, tx *sql.Tx, in MoveIssueProjectIn) ([]IssueMoveTreeItem, error) {
	if in.Single {
		inputs := []IssueMoveTreeItem{{In: in}}
		if err := validateIssueMoveMembership(in.ExpectedIssueIDs, inputs); err != nil {
			return nil, err
		}
		if in.AuthorizeProjects != nil {
			if err := authorizeIssueMoveProjects(ctx, tx, in, []int64{in.FromProjectID}); err != nil {
				return nil, err
			}
		}
		return inputs, nil
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE tree(id) AS (
 SELECT id FROM issues WHERE id=$1
 UNION
 SELECT l.from_issue_id FROM links l JOIN tree t ON l.to_issue_id=t.id
 JOIN issues child ON child.id=l.from_issue_id
 WHERE l.type='parent' AND child.deleted_at IS NULL
 ) SELECT i.id, i.project_id, i.revision, i.uid, i.short_id, p.uid, p.name, p.deleted_at IS NOT NULL
 FROM issues i JOIN tree t ON t.id=i.id JOIN projects p ON p.id=i.project_id
 WHERE i.deleted_at IS NULL ORDER BY CASE WHEN i.id=$1 THEN 0 ELSE 1 END, i.id`, in.IssueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []IssueMoveTreeItem
	var archived []IssueMoveDescendant
	for rows.Next() {
		item := in
		var descendant IssueMoveDescendant
		var projectArchived bool
		if err := rows.Scan(&item.IssueID, &item.FromProjectID, &item.IfMatchRev,
			&descendant.UID, &descendant.ShortID, &descendant.ProjectUID, &descendant.ProjectName, &projectArchived); err != nil {
			return nil, err
		}
		if item.IssueID == in.IssueID {
			items = append(items, IssueMoveTreeItem{In: in})
			continue
		}
		if item.FromProjectID == in.ToProjectID {
			continue
		}
		if projectArchived {
			archived = append(archived, descendant)
		}
		items = append(items, IssueMoveTreeItem{In: item, Descendant: &descendant})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 || items[0].In.IssueID != in.IssueID {
		return nil, fmt.Errorf("issue %d not in project %d: %w", in.IssueID, in.FromProjectID, ErrNotFound)
	}
	if err := validateIssueMoveMembership(in.ExpectedIssueIDs, items); err != nil {
		return nil, err
	}
	if in.AuthorizeProjects != nil {
		ids := make([]int64, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.In.FromProjectID)
		}
		if err := authorizeIssueMoveProjects(ctx, tx, in, ids); err != nil {
			return nil, err
		}
	}
	// Report archived projects only after the caller may see every source project.
	if len(archived) > 0 {
		return nil, &IssueMoveDescendantError{Descendant: archived[0], Err: ErrIssueMoveDescendantArchived}
	}
	if err := validateIssueMoveTreeMappings(ctx, tx, items, in.ToProjectID); err != nil {
		return nil, err
	}
	return items, nil
}

func validateIssueMoveMembership(expected []int64, inputs []IssueMoveTreeItem) error {
	if expected == nil {
		return nil
	}
	want := make(map[int64]struct{}, len(expected))
	for _, id := range expected {
		want[id] = struct{}{}
	}
	if len(want) != len(inputs) {
		return ErrIssueMoveTreeChanged
	}
	for _, item := range inputs {
		if _, ok := want[item.In.IssueID]; !ok {
			return ErrIssueMoveTreeChanged
		}
	}
	return nil
}

// Validate the complete proposed mapping set before previews or writes. Checking
// each issue against the unchanged target misses collisions within the subtree.
func validateIssueMoveTreeMappings(ctx context.Context, tx *sql.Tx, items []IssueMoveTreeItem, targetID int64) error {
	predicates := []string{"project_id=$1"}
	args := []any{targetID}
	for _, item := range items {
		predicates = append(predicates, fmt.Sprintf("(issue_id=$%d AND project_id=$%d)", len(args)+1, len(args)+2))
		args = append(args, item.In.IssueID, item.In.FromProjectID)
	}
	//nolint:gosec // Predicates contain generated placeholders; all project and issue IDs are bound parameters.
	rows, err := tx.QueryContext(ctx, `SELECT source, external_id, object_type FROM import_mappings
 WHERE `+strings.Join(predicates, " OR ")+`
 GROUP BY source, external_id, object_type HAVING COUNT(*)>1
 ORDER BY source, object_type, external_id LIMIT 20`, args...)
	if err != nil {
		return fmt.Errorf("check subtree import mapping collisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var collisions []ProjectMergeImportMappingCollision
	for rows.Next() {
		var collision ProjectMergeImportMappingCollision
		if err := rows.Scan(&collision.Source, &collision.ExternalID, &collision.ObjectType); err != nil {
			return err
		}
		collisions = append(collisions, collision)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(collisions) > 0 {
		return &ProjectMergeImportMappingCollisionError{Mappings: collisions}
	}
	return nil
}

// authorizeIssueMoveProjects expands admission and then rechecks its updated
// transaction fence. Rejected fences finish host cleanup only after rollback.
func authorizeIssueMoveProjects(ctx context.Context, tx *sql.Tx, in MoveIssueProjectIn, ids []int64) error {
	uids := make([]string, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		var uid string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, id).Scan(&uid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		seen[id] = true
		uids = append(uids, uid)
	}
	err := in.AuthorizeProjects(uids)
	if err == nil {
		err = ApplyTransactionFence(ctx, tx)
	}
	if err != nil {
		return FinishTransactionRollback(ctx, err, tx.Rollback())
	}
	return nil
}

// MovedIssueUID resolves a historical short ID from the existing move journal.
// It follows the stable issue UID even after multiple moves. Compatibility is
// history-scoped: purging move history or merging a former project can retire
// these aliases. The move returns the current reference; stable UIDs do not change.
// Local issues (including deleted ones) and purge tombstones take precedence over
// history because an older allocator may have reused a moved issue's short ID.
func MovedIssueUID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID int64, shortID string, postgres bool) (string, error) {
	fromProject := "json_extract(e.payload, '$.from_project_uid')"
	fromShort := "json_extract(e.payload, '$.from_short_id')"
	moveEvent := "json_extract(e.payload, '$.move_event_uid')"
	if postgres {
		fromProject = "e.payload::jsonb->>'from_project_uid'"
		fromShort = "e.payload::jsonb->>'from_short_id'"
		moveEvent = "e.payload::jsonb->>'move_event_uid'"
	}
	// idx_events_moved_from finds a former short ID directly; a full UID uses
	// the issue history index. Issue creation runs this reservation check, so
	// neither form may scan issues or events. Expressions must match the index.
	match := fromShort + "=$2"
	if len(shortID) == shortid.MaxLength {
		match = "e.issue_uid=upper($2)"
	}
	// Bind the project UID: SQLite cannot search an expression index with a
	// value produced by a subquery.
	var projectUID string
	err := q.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var resetUID string
	var resetID int64
	err = q.QueryRowContext(ctx, `SELECT e.issue_uid, e.id FROM events e
 WHERE e.project_id=$1 AND e.type='sync.reset_required' AND `+fromProject+`=$3 AND `+match+`
 AND EXISTS (SELECT 1 FROM events retained_move WHERE retained_move.uid=`+moveEvent+`
 AND retained_move.type='issue.moved' AND retained_move.issue_uid=e.issue_uid)
 AND EXISTS (SELECT 1 FROM issues WHERE uid=e.issue_uid)
 AND NOT EXISTS (SELECT 1 FROM issues WHERE project_id=$1 AND short_id=$2)
 AND NOT EXISTS (SELECT 1 FROM purge_log WHERE project_id=$1 AND short_id=$2)
 ORDER BY e.id DESC LIMIT 1`, projectID, shortID, projectUID).Scan(&resetUID, &resetID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var uid string
	var moveID int64
	err = q.QueryRowContext(ctx, `SELECT e.issue_uid, e.id FROM events e
 WHERE e.type='issue.moved' AND `+fromProject+`=$3 AND `+match+`
 AND EXISTS (SELECT 1 FROM issues WHERE uid=e.issue_uid)
 AND NOT EXISTS (SELECT 1 FROM issues WHERE project_id=$1 AND short_id=$2)
 AND NOT EXISTS (SELECT 1 FROM purge_log WHERE project_id=$1 AND short_id=$2)
 ORDER BY e.id DESC LIMIT 1`, projectID, shortID, projectUID).Scan(&uid, &moveID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if resetID > moveID {
		return resetUID, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return uid, nil
}
