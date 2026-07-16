package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

const labelSelect = `SELECT issue_id, label, author, created_at FROM issue_labels`

var labelConstraintErrors = map[string]error{
	"issue_labels_pkey":                db.ErrLabelExists,
	"issue_labels_label_length_check":  db.ErrLabelInvalid,
	"issue_labels_label_charset_check": db.ErrLabelInvalid,
	"issue_labels_label_check":         db.ErrLabelInvalid,
	"issue_labels_label_check1":        db.ErrLabelInvalid,
}

// AddLabel attaches a label without emitting an event.
func (s *Store) AddLabel(ctx context.Context, issueID int64, label, author string) (db.IssueLabel, error) {
	var issueLabel db.IssueLabel
	err := s.RetryTransient(ctx, func() error {
		var err error
		issueLabel, err = scanLabel(s.QueryRowContext(ctx,
			`INSERT INTO issue_labels(issue_id, label, author) VALUES($1,$2,$3)
			 RETURNING issue_id, label, author, created_at`, issueID, label, author))
		return mapSQLError(err, labelConstraintErrors)
	})
	return issueLabel, err
}

// AddLabelAndEvent attaches a label, emits its event, and touches the issue in
// one transaction.
func (s *Store) AddLabelAndEvent(
	ctx context.Context,
	issueID int64,
	params db.LabelEventParams,
) (db.IssueLabel, db.Event, error) {
	var issueLabel db.IssueLabel
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, project, err := lockedIssueTx(ctx, tx, issueID, false)
		if err != nil {
			return err
		}
		params.Actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Actor)
		if err != nil {
			return err
		}
		issueLabel, err = scanLabel(tx.QueryRowContext(ctx,
			`INSERT INTO issue_labels(issue_id, label, author) VALUES($1,$2,$3)
			 RETURNING issue_id, label, author, created_at`, issue.ID, params.Label, params.Actor))
		if err != nil {
			return mapSQLError(err, labelConstraintErrors)
		}
		updatedAt := nowStoredTimestamp()
		payload, err := json.Marshal(map[string]string{
			"issue_uid":  issue.UID,
			"label":      params.Label,
			"updated_at": updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal label payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx,
			issueEventInput(issue, project, params.EventType, params.Actor, string(payload)))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = $1 WHERE id = $2`, updatedAt, issue.ID)
		return mapSQLError(err, nil)
	})
	return issueLabel, event, err
}

// RemoveLabel detaches a label without emitting an event.
func (s *Store) RemoveLabel(ctx context.Context, issueID int64, label string) error {
	return s.RetryTransient(ctx, func() error {
		result, err := s.ExecContext(ctx,
			`DELETE FROM issue_labels WHERE issue_id = $1 AND label = $2`, issueID, label)
		if err != nil {
			return mapSQLError(err, nil)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return db.ErrNotFound
		}
		return nil
	})
}

// RemoveLabelAndEvent detaches a label and records the mutation atomically.
func (s *Store) RemoveLabelAndEvent(
	ctx context.Context,
	issueID int64,
	params db.LabelEventParams,
) (db.Event, error) {
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, project, err := lockedIssueTx(ctx, tx, issueID, false)
		if err != nil {
			return err
		}
		params.Actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Actor)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`DELETE FROM issue_labels WHERE issue_id = $1 AND label = $2`, issue.ID, params.Label)
		if err != nil {
			return mapSQLError(err, nil)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return db.ErrNotFound
		}
		updatedAt := nowStoredTimestamp()
		payload, err := json.Marshal(map[string]string{
			"issue_uid":  issue.UID,
			"label":      params.Label,
			"updated_at": updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal label payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx,
			issueEventInput(issue, project, params.EventType, params.Actor, string(payload)))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = $1 WHERE id = $2`, updatedAt, issue.ID)
		return mapSQLError(err, nil)
	})
	return event, err
}

// HasLabel reports whether one label is attached.
func (s *Store) HasLabel(ctx context.Context, issueID int64, label string) (bool, error) {
	var present bool
	if err := s.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM issue_labels WHERE issue_id = $1 AND label = $2)`,
		issueID, label,
	).Scan(&present); err != nil {
		return false, mapSQLError(err, nil)
	}
	return present, nil
}

// LabelByEndpoints returns one attached label.
func (s *Store) LabelByEndpoints(ctx context.Context, issueID int64, label string) (db.IssueLabel, error) {
	return scanLabel(s.QueryRowContext(ctx,
		labelSelect+` WHERE issue_id = $1 AND label = $2`, issueID, label))
}

// LabelCounts returns per-label counts for active issues in one project.
func (s *Store) LabelCounts(ctx context.Context, projectID int64) ([]db.LabelCount, error) {
	rows, err := s.QueryContext(ctx, `SELECT il.label, COUNT(*)
		FROM issue_labels il
		JOIN issues i ON i.id = il.issue_id
		WHERE i.project_id = $1 AND i.deleted_at IS NULL
		GROUP BY il.label
		ORDER BY COUNT(*) DESC, il.label ASC`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var counts []db.LabelCount
	for rows.Next() {
		var count db.LabelCount
		if err := rows.Scan(&count.Label, &count.Count); err != nil {
			return nil, mapSQLError(err, nil)
		}
		counts = append(counts, count)
	}
	return counts, mapSQLError(rows.Err(), nil)
}

// LabelsByIssue returns label rows in alphabetical order.
func (s *Store) LabelsByIssue(ctx context.Context, issueID int64) ([]db.IssueLabel, error) {
	rows, err := s.QueryContext(ctx,
		labelSelect+` WHERE issue_id = $1 ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var labels []db.IssueLabel
	for rows.Next() {
		label, err := scanLabel(rows)
		if err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, mapSQLError(rows.Err(), nil)
}

// LabelsByIssues returns an issue-to-label map constrained to one project.
func (s *Store) LabelsByIssues(
	ctx context.Context,
	projectID int64,
	issueIDs []int64,
) (map[int64][]string, error) {
	labels := map[int64][]string{}
	if len(issueIDs) == 0 {
		return labels, nil
	}
	rows, err := s.QueryContext(ctx, `SELECT il.issue_id, il.label
		FROM issue_labels il
		JOIN issues i ON i.id = il.issue_id
		WHERE i.project_id = $1 AND il.issue_id = ANY($2::bigint[])
		ORDER BY il.issue_id ASC, il.label ASC`, projectID, issueIDs)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var issueID int64
		var label string
		if err := rows.Scan(&issueID, &label); err != nil {
			return nil, mapSQLError(err, nil)
		}
		labels[issueID] = append(labels[issueID], label)
	}
	return labels, mapSQLError(rows.Err(), nil)
}

// LabelsForIssue returns only sorted label values for hook expansion.
func (s *Store) LabelsForIssue(ctx context.Context, issueID int64) ([]string, error) {
	rows, err := s.QueryContext(ctx,
		`SELECT label FROM issue_labels WHERE issue_id = $1 ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, mapSQLError(err, nil)
		}
		labels = append(labels, label)
	}
	return labels, mapSQLError(rows.Err(), nil)
}

func scanLabel(row rowScanner) (db.IssueLabel, error) {
	var label db.IssueLabel
	var createdAt string
	err := row.Scan(&label.IssueID, &label.Label, &label.Author, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueLabel{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueLabel{}, mapSQLError(err, nil)
	}
	label.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.IssueLabel{}, fmt.Errorf("parse label created_at: %w", err)
	}
	return label, nil
}
