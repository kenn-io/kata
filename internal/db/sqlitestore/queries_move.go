package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

// MoveIssueProject moves an issue from one project to another within the same
// database, allocating a fresh short_id in the target project and emitting an
// issue.moved event. It refuses if:
//   - source and target projects are the same
//   - IfMatchRev does not match the current revision (RevisionConflictError)
//   - the issue belongs to a recurrence series (RecurrencePinnedError)
//   - any link is anchored on the issue (CrossProjectLinksError)
func (d *Store) MoveIssueProject(ctx context.Context, in db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	var out db.MoveIssueProjectOut
	if in.FromProjectID == in.ToProjectID {
		return out, fmt.Errorf("source and target projects are the same")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		curRev         int64
		curShortID     string
		recurrenceID   *int64
		issueUID       string
		fromProjectUID string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT i.revision, i.short_id, i.recurrence_id, i.uid, p.uid
		  FROM issues i JOIN projects p ON p.id = i.project_id
		 WHERE i.id = ? AND i.project_id = ? AND i.deleted_at IS NULL`,
		in.IssueID, in.FromProjectID,
	).Scan(&curRev, &curShortID, &recurrenceID, &issueUID, &fromProjectUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("issue %d not in project %d", in.IssueID, in.FromProjectID)
		}
		return out, err
	}
	if err := ensureFederatedMoveAllowedTx(ctx, tx, in.FromProjectID, in.ToProjectID); err != nil {
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.FromProjectID); err != nil {
		return out, err
	}
	if in.IfMatchRev != curRev {
		return out, &db.RevisionConflictError{CurrentRevision: curRev}
	}
	if recurrenceID != nil {
		return out, &db.RecurrencePinnedError{}
	}

	blockers, err := d.findLinksTx(ctx, tx, in.IssueID)
	if err != nil {
		return out, err
	}
	if len(blockers) > 0 {
		return out, &db.CrossProjectLinksError{Blockers: blockers}
	}

	var (
		toProjectUID  string
		toProjectName string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT uid, name FROM projects WHERE id = ? AND deleted_at IS NULL`,
		in.ToProjectID,
	).Scan(&toProjectUID, &toProjectName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("target project %d not found", in.ToProjectID)
		}
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.ToProjectID); err != nil {
		return out, err
	}

	newShortID, err := assignShortIDIn(ctx, tx,
		[]int64{in.ToProjectID}, issueUID, shortid.MinLength,
	)
	if err != nil {
		return out, fmt.Errorf("allocate short_id in target: %w", err)
	}

	newRev := curRev + 1
	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx, `
		UPDATE issues
		   SET project_id = ?,
		       short_id   = ?,
		       revision   = ?,
		       updated_at = ?
		 WHERE id = ?`,
		in.ToProjectID, newShortID, newRev, ts, in.IssueID,
	); err != nil {
		return out, err
	}

	// Rehome import_mappings rows for the moved issue. The UNIQUE constraint on
	// import_mappings is (source, external_id, object_type, project_id). If the
	// target project already has a row for the same (source, external_id,
	// object_type), the UPDATE would violate UNIQUE — collect the colliding IDs
	// and delete them first (the target mapping is already authoritative).
	type collisionKey struct {
		source, externalID, objectType string
	}
	collisionRows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.source, m.external_id, m.object_type
		  FROM import_mappings m
		 WHERE m.issue_id = ? AND m.project_id = ?
		   AND EXISTS (
		       SELECT 1 FROM import_mappings t
		        WHERE t.project_id  = ?
		          AND t.source      = m.source
		          AND t.external_id = m.external_id
		          AND t.object_type = m.object_type
		   )`,
		in.IssueID, in.FromProjectID, in.ToProjectID,
	)
	if err != nil {
		return out, fmt.Errorf("find colliding import_mappings: %w", err)
	}
	var collidingIDs []int64
	for collisionRows.Next() {
		var id int64
		var k collisionKey
		if err := collisionRows.Scan(&id, &k.source, &k.externalID, &k.objectType); err != nil {
			_ = collisionRows.Close()
			return out, fmt.Errorf("scan colliding import_mappings: %w", err)
		}
		collidingIDs = append(collidingIDs, id)
	}
	if err := collisionRows.Close(); err != nil {
		return out, fmt.Errorf("close collision rows: %w", err)
	}
	if err := collisionRows.Err(); err != nil {
		return out, fmt.Errorf("iterate collision rows: %w", err)
	}
	for _, id := range collidingIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id = ?`, id); err != nil {
			return out, fmt.Errorf("drop colliding import_mapping %d: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE import_mappings
		   SET project_id = ?
		 WHERE issue_id = ? AND project_id = ?`,
		in.ToProjectID, in.IssueID, in.FromProjectID,
	); err != nil {
		return out, fmt.Errorf("rehome import_mappings: %w", err)
	}

	payload, _ := json.Marshal(map[string]string{
		"issue_uid":        issueUID,
		"from_project_uid": fromProjectUID,
		"from_short_id":    curShortID,
		"to_project_uid":   toProjectUID,
		"to_short_id":      newShortID,
		"updated_at":       ts,
	})
	ev, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   in.ToProjectID,
		ProjectName: toProjectName,
		IssueID:     &in.IssueID,
		IssueUID:    &issueUID,
		Type:        "issue.moved",
		Actor:       in.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return out, err
	}

	if err := tx.Commit(); err != nil {
		return out, err
	}

	issue, err := d.IssueByID(ctx, in.IssueID)
	if err != nil {
		return out, err
	}
	out.Issue = issue
	out.EventID = ev.ID
	out.NewShortID = newShortID
	out.NewRevision = newRev
	return out, nil
}

// findLinksTx returns all links involving issueID (as either endpoint),
// used to detect anchored links that would become cross-project after a move.
func (d *Store) findLinksTx(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.LinkBlocker, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT l.id, l.type,
		       CASE WHEN l.from_issue_id = ? THEN l.to_issue_uid ELSE l.from_issue_uid END AS peer_uid
		  FROM links l
		 WHERE l.from_issue_id = ? OR l.to_issue_id = ?`,
		issueID, issueID, issueID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []db.LinkBlocker
	for rows.Next() {
		var b db.LinkBlocker
		if err := rows.Scan(&b.LinkID, &b.Type, &b.PeerUID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
