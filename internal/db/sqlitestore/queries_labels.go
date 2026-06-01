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

// AddLabel attaches a label to an issue.
func (d *Store) AddLabel(ctx context.Context, issueID int64, label, author string) (db.IssueLabel, error) {
	if _, err := d.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
		issueID, label, author); err != nil {
		return db.IssueLabel{}, classifyLabelInsertError(err)
	}
	row := d.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`, issueID, label)
	out, err := scanLabel(row)
	if err != nil {
		return db.IssueLabel{}, fmt.Errorf("re-fetch label: %w", err)
	}
	return out, nil
}

func classifyLabelInsertError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: issue_labels.issue_id, issue_labels.label"):
		return db.ErrLabelExists
	case strings.Contains(msg, "CHECK constraint failed") &&
		(strings.Contains(msg, "length(label)") || strings.Contains(msg, "label NOT GLOB")):
		// Scoped to the two label-related CHECKs (length BETWEEN 1 AND 64
		// and the charset GLOB). Other CHECKs on the table (e.g. blank
		// author) fall through to the wrapped generic error rather than
		// being misreported as invalid labels.
		return db.ErrLabelInvalid
	}
	return fmt.Errorf("insert label: %w", err)
}

// RemoveLabel detaches a label from an issue. Returns ErrNotFound when the row
// doesn't exist (idempotent unlink semantics live in the handler).
func (d *Store) RemoveLabel(ctx context.Context, issueID int64, label string) error {
	res, err := d.ExecContext(ctx,
		`DELETE FROM issue_labels WHERE issue_id = ? AND label = ?`,
		issueID, label)
	if err != nil {
		return fmt.Errorf("delete label: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete label rows affected: %w", err)
	}
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

// HasLabel reports whether (issueID, label) exists.
func (d *Store) HasLabel(ctx context.Context, issueID int64, label string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT 1 FROM issue_labels WHERE issue_id = ? AND label = ?`,
		issueID, label).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("has label: %w", err)
	}
	return n == 1, nil
}

// LabelByEndpoints fetches the label row for (issueID, label). Returns
// ErrNotFound when the label is not attached to the issue.
func (d *Store) LabelByEndpoints(ctx context.Context, issueID int64, label string) (db.IssueLabel, error) {
	row := d.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`,
		issueID, label)
	return scanLabel(row)
}

// LabelsByIssue returns every label attached to issueID, ordered alphabetically.
func (d *Store) LabelsByIssue(ctx context.Context, issueID int64) ([]db.IssueLabel, error) {
	rows, err := d.QueryContext(ctx,
		labelSelect+` WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.IssueLabel
	for rows.Next() {
		l, err := scanLabel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LabelCounts returns the per-label aggregate for projectID, excluding
// soft-deleted issues.
func (d *Store) LabelCounts(ctx context.Context, projectID int64) ([]db.LabelCount, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT il.label, COUNT(*) AS n
		 FROM issue_labels il
		 JOIN issues i ON i.id = il.issue_id
		 WHERE i.project_id = ? AND i.deleted_at IS NULL
		 GROUP BY il.label
		 ORDER BY n DESC, il.label ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("label counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.LabelCount
	for rows.Next() {
		var c db.LabelCount
		if err := rows.Scan(&c.Label, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddLabelAndEvent attaches a label to an issue, emits the matching
// issue.labeled event, and bumps the issue's updated_at — all in one TX.
// Returns the new label row and the event row. Typed errors (ErrLabelExists,
// ErrLabelInvalid) flow up unchanged from the underlying INSERT classification.
//
// Used by the daemon's POST /labels handler so the label insert and its event
// are atomic — there's no window where the row exists without an event.
func (d *Store) AddLabelAndEvent(ctx context.Context, issueID int64, ev db.LabelEventParams) (db.IssueLabel, db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.IssueLabel{}, db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
		issueID, ev.Label, ev.Actor); err != nil {
		return db.IssueLabel{}, db.Event{}, classifyLabelInsertError(err)
	}

	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]string{
		"issue_uid":  issue.UID,
		"label":      ev.Label,
		"updated_at": ts,
	})
	if err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        ev.EventType,
		Actor:       ev.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.IssueLabel{}, db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, issueID); err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("touch issue: %w", err)
	}

	// Re-fetch the inserted row INSIDE the TX so a post-commit failure
	// (context cancellation, concurrent removal) can't leave the caller with
	// a 500 after the mutation has already committed.
	out, err := scanLabel(tx.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`, issueID, ev.Label))
	if err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("re-fetch label inside tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return db.IssueLabel{}, db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return out, evt, nil
}

// RemoveLabelAndEvent detaches a label and emits the matching issue.unlabeled
// event in one TX. Returns ErrNotFound when the label was never attached —
// caller maps to 200 no-op envelope per spec §4.5.
func (d *Store) RemoveLabelAndEvent(ctx context.Context, issueID int64, ev db.LabelEventParams) (db.Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Event{}, err
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM issue_labels WHERE issue_id = ? AND label = ?`,
		issueID, ev.Label)
	if err != nil {
		return db.Event{}, fmt.Errorf("delete label: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.Event{}, fmt.Errorf("delete label rows affected: %w", err)
	}
	if n == 0 {
		return db.Event{}, db.ErrNotFound
	}

	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]string{
		"issue_uid":  issue.UID,
		"label":      ev.Label,
		"updated_at": ts,
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        ev.EventType,
		Actor:       ev.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, issueID); err != nil {
		return db.Event{}, fmt.Errorf("touch issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.Event{}, fmt.Errorf("commit: %w", err)
	}
	return evt, nil
}

const labelSelect = `SELECT issue_id, label, author, created_at FROM issue_labels`

func scanLabel(r rowScanner) (db.IssueLabel, error) {
	var l db.IssueLabel
	err := r.Scan(&l.IssueID, &l.Label, &l.Author, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueLabel{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueLabel{}, fmt.Errorf("scan label: %w", err)
	}
	return l, nil
}
