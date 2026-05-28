package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrLabelExists is returned when (issue_id, label) already exists.
// Caller treats this as a no-op success on duplicate labels.
var ErrLabelExists = errors.New("label already attached")

// ErrLabelInvalid is returned when the label fails the schema's charset/length
// CHECK constraint.
var ErrLabelInvalid = errors.New("invalid label")

// AddLabel attaches a label to an issue.
func (d *DB) AddLabel(ctx context.Context, issueID int64, label, author string) (IssueLabel, error) {
	if _, err := d.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
		issueID, label, author); err != nil {
		return IssueLabel{}, classifyLabelInsertError(err)
	}
	row := d.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`, issueID, label)
	out, err := scanLabel(row)
	if err != nil {
		return IssueLabel{}, fmt.Errorf("re-fetch label: %w", err)
	}
	return out, nil
}

func classifyLabelInsertError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: issue_labels.issue_id, issue_labels.label"):
		return ErrLabelExists
	case strings.Contains(msg, "CHECK constraint failed") &&
		(strings.Contains(msg, "length(label)") || strings.Contains(msg, "label NOT GLOB")):
		// Scoped to the two label-related CHECKs (length BETWEEN 1 AND 64
		// and the charset GLOB). Other CHECKs on the table (e.g. blank
		// author) fall through to the wrapped generic error rather than
		// being misreported as invalid labels.
		return ErrLabelInvalid
	}
	return fmt.Errorf("insert label: %w", err)
}

// RemoveLabel detaches a label from an issue. Returns ErrNotFound when the row
// doesn't exist (idempotent unlink semantics live in the handler).
func (d *DB) RemoveLabel(ctx context.Context, issueID int64, label string) error {
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
		return ErrNotFound
	}
	return nil
}

// HasLabel reports whether (issueID, label) exists.
func (d *DB) HasLabel(ctx context.Context, issueID int64, label string) (bool, error) {
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
func (d *DB) LabelByEndpoints(ctx context.Context, issueID int64, label string) (IssueLabel, error) {
	row := d.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`,
		issueID, label)
	return scanLabel(row)
}

// LabelsByIssue returns every label attached to issueID, ordered alphabetically.
func (d *DB) LabelsByIssue(ctx context.Context, issueID int64) ([]IssueLabel, error) {
	rows, err := d.QueryContext(ctx,
		labelSelect+` WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []IssueLabel
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
func (d *DB) LabelCounts(ctx context.Context, projectID int64) ([]LabelCount, error) {
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
	var out []LabelCount
	for rows.Next() {
		var c LabelCount
		if err := rows.Scan(&c.Label, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LabelEventParams describes the event-emission side of a label mutation. The
// DB-layer methods AddLabelAndEvent and RemoveLabelAndEvent split the mutation
// (label insert/delete) from the event metadata so the handler can emit the
// matching issue.labeled / issue.unlabeled event without an extra round trip.
type LabelEventParams struct {
	EventType string // "issue.labeled" | "issue.unlabeled"
	Label     string // the label being added/removed (used for both DB op and event payload)
	Actor     string
}

// AddLabelAndEvent attaches a label to an issue, emits the matching
// issue.labeled event, and bumps the issue's updated_at — all in one TX.
// Returns the new label row and the event row. Typed errors (ErrLabelExists,
// ErrLabelInvalid) flow up unchanged from the underlying INSERT classification.
//
// Used by the daemon's POST /labels handler so the label insert and its event
// are atomic — there's no window where the row exists without an event.
func (d *DB) AddLabelAndEvent(ctx context.Context, issueID int64, ev LabelEventParams) (IssueLabel, Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return IssueLabel{}, Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return IssueLabel{}, Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, ?)`,
		issueID, ev.Label, ev.Actor); err != nil {
		return IssueLabel{}, Event{}, classifyLabelInsertError(err)
	}

	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]string{
		"issue_uid":  issue.UID,
		"label":      ev.Label,
		"updated_at": ts,
	})
	if err != nil {
		return IssueLabel{}, Event{}, fmt.Errorf("marshal label payload: %w", err)
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
		return IssueLabel{}, Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, issueID); err != nil {
		return IssueLabel{}, Event{}, fmt.Errorf("touch issue: %w", err)
	}

	// Re-fetch the inserted row INSIDE the TX so a post-commit failure
	// (context cancellation, concurrent removal) can't leave the caller with
	// a 500 after the mutation has already committed.
	out, err := scanLabel(tx.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = ? AND label = ?`, issueID, ev.Label))
	if err != nil {
		return IssueLabel{}, Event{}, fmt.Errorf("re-fetch label inside tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return IssueLabel{}, Event{}, fmt.Errorf("commit: %w", err)
	}
	return out, evt, nil
}

// RemoveLabelAndEvent detaches a label and emits the matching issue.unlabeled
// event in one TX. Returns ErrNotFound when the label was never attached —
// caller maps to 200 no-op envelope per spec §4.5.
func (d *DB) RemoveLabelAndEvent(ctx context.Context, issueID int64, ev LabelEventParams) (Event, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return Event{}, err
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM issue_labels WHERE issue_id = ? AND label = ?`,
		issueID, ev.Label)
	if err != nil {
		return Event{}, fmt.Errorf("delete label: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Event{}, fmt.Errorf("delete label rows affected: %w", err)
	}
	if n == 0 {
		return Event{}, ErrNotFound
	}

	ts := nowTimestamp()
	payload, err := json.Marshal(map[string]string{
		"issue_uid":  issue.UID,
		"label":      ev.Label,
		"updated_at": ts,
	})
	if err != nil {
		return Event{}, fmt.Errorf("marshal label payload: %w", err)
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
		return Event{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = ? WHERE id = ?`,
		ts, issueID); err != nil {
		return Event{}, fmt.Errorf("touch issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit: %w", err)
	}
	return evt, nil
}

const labelSelect = `SELECT issue_id, label, author, created_at FROM issue_labels`

func scanLabel(r rowScanner) (IssueLabel, error) {
	var l IssueLabel
	err := r.Scan(&l.IssueID, &l.Label, &l.Author, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IssueLabel{}, ErrNotFound
	}
	if err != nil {
		return IssueLabel{}, fmt.Errorf("scan label: %w", err)
	}
	return l, nil
}
