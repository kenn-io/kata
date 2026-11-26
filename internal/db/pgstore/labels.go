package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
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
		issueLabel, err = fencedQueryRow(ctx, s, scanLabel,
			`INSERT INTO issue_labels(issue_id, label, author) VALUES($1,$2,$3)
			 RETURNING issue_id, label, author, created_at`, issueID, label, author)
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
		if params.IfMatchRev != nil && *params.IfMatchRev != issue.Revision {
			return &db.RevisionConflictError{CurrentRevision: issue.Revision}
		}
		params.Actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Actor)
		if err != nil {
			return err
		}
		updatedAt := nowStoredTimestamp()
		issueLabel, event, err = s.attachLabelTx(ctx, tx, issue, project, params.EventType, params.Label, params.Actor, updatedAt)
		if err != nil {
			return err
		}
		return touchIssueTx(ctx, tx, issue.ID, updatedAt)
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
		if params.IfMatchRev != nil && *params.IfMatchRev != issue.Revision {
			return &db.RevisionConflictError{CurrentRevision: issue.Revision}
		}
		params.Actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Actor)
		if err != nil {
			return err
		}
		updatedAt := nowStoredTimestamp()
		event, err = s.detachLabelTx(ctx, tx, issue, project, params.EventType, params.Label, params.Actor, updatedAt)
		if err != nil {
			return err
		}
		return touchIssueTx(ctx, tx, issue.ID, updatedAt)
	})
	return event, err
}

// ChangeLabelsAndEvents removes then adds labels on one issue in a single
// transaction. Removes of absent labels and adds of attached labels are
// skipped without an event; any other failure, including an invalid label,
// rolls everything back.
func (s *Store) ChangeLabelsAndEvents(
	ctx context.Context,
	issueID int64,
	params db.ChangeLabelsParams,
) ([]string, []db.Event, error) {
	var labels []string
	var events []db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		// The callback may run again on a serialization retry.
		labels = []string{}
		events = make([]db.Event, 0, len(params.Remove)+len(params.Add))
		issue, project, err := lockedIssueTx(ctx, tx, issueID, false)
		if err != nil {
			return err
		}
		if params.IfMatchRev != nil && *params.IfMatchRev != issue.Revision {
			return &db.RevisionConflictError{CurrentRevision: issue.Revision}
		}
		actor, err := effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Actor)
		if err != nil {
			return err
		}
		updatedAt := nowStoredTimestamp()
		for _, label := range params.Remove {
			event, err := s.detachLabelTx(ctx, tx, issue, project, "issue.unlabeled", label, actor, updatedAt)
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			events = append(events, event)
		}
		for _, label := range params.Add {
			_, event, err := s.attachLabelTx(ctx, tx, issue, project, "issue.labeled", label, actor, updatedAt)
			if errors.Is(err, db.ErrLabelExists) {
				continue
			}
			if err != nil {
				return err
			}
			events = append(events, event)
		}
		if len(events) > 0 {
			if err := touchIssueTx(ctx, tx, issue.ID, updatedAt); err != nil {
				return err
			}
		}
		labels, err = federationIssueLabels(ctx, tx, issue.ID)
		if labels == nil {
			labels = []string{}
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return labels, events, nil
}

// attachLabelTx inserts the label row and its event. ON CONFLICT keeps a
// duplicate from aborting the PostgreSQL transaction; it returns
// db.ErrLabelExists instead.
func (s *Store) attachLabelTx(
	ctx context.Context,
	tx *sql.Tx,
	issue db.Issue,
	project db.Project,
	eventType, label, actor, updatedAt string,
) (db.IssueLabel, db.Event, error) {
	issueLabel, err := scanLabel(tx.QueryRowContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author) VALUES($1,$2,$3)
		 ON CONFLICT DO NOTHING
		 RETURNING issue_id, label, author, created_at`, issue.ID, label, actor))
	if errors.Is(err, db.ErrNotFound) {
		return db.IssueLabel{}, db.Event{}, db.ErrLabelExists
	}
	if err != nil {
		return db.IssueLabel{}, db.Event{}, mapSQLError(err, labelConstraintErrors)
	}
	event, err := s.insertLabelEventTx(ctx, tx, issue, project, eventType, label, actor, updatedAt)
	return issueLabel, event, err
}

// detachLabelTx deletes the label row and records its event. It returns
// db.ErrNotFound when the label was not attached.
func (s *Store) detachLabelTx(
	ctx context.Context,
	tx *sql.Tx,
	issue db.Issue,
	project db.Project,
	eventType, label, actor, updatedAt string,
) (db.Event, error) {
	result, err := tx.ExecContext(ctx,
		`DELETE FROM issue_labels WHERE issue_id = $1 AND label = $2`, issue.ID, label)
	if err != nil {
		return db.Event{}, mapSQLError(err, nil)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return db.Event{}, err
	}
	if count == 0 {
		return db.Event{}, db.ErrNotFound
	}
	return s.insertLabelEventTx(ctx, tx, issue, project, eventType, label, actor, updatedAt)
}

func (s *Store) insertLabelEventTx(
	ctx context.Context,
	tx *sql.Tx,
	issue db.Issue,
	project db.Project,
	eventType, label, actor, updatedAt string,
) (db.Event, error) {
	payload, err := json.Marshal(map[string]string{
		"issue_uid":  issue.UID,
		"label":      label,
		"updated_at": updatedAt,
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	return s.insertEventTx(ctx, tx,
		issueEventInput(issue, project, eventType, actor, string(payload)))
}

func touchIssueTx(ctx context.Context, tx *sql.Tx, issueID int64, updatedAt string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE issues SET updated_at = $1, revision = revision + 1 WHERE id = $2`, updatedAt, issueID)
	return mapSQLError(err, nil)
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
