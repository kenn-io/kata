package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/recurrence"
	katauid "go.kenn.io/kata/internal/uid"
)

const recurrenceSelect = `SELECT r.id, r.uid, r.project_id, r.rrule, r.dtstart, r.timezone,
       r.template_title, r.template_body, r.template_owner, r.template_priority,
       r.template_labels, r.template_metadata, r.next_occurrence_key,
       r.last_materialized_uid, r.author, r.revision, r.created_at, r.updated_at, r.deleted_at
  FROM recurrences r`

type recurrenceDiffEntry struct {
	From json.RawMessage `json:"from"`
	To   json.RawMessage `json:"to"`
}

// CreateRecurrence persists a validated recurrence and its creation event.
func (s *Store) CreateRecurrence(ctx context.Context, input db.CreateRecurrenceIn) (db.Recurrence, error) {
	labels, err := db.NormalizeRecurrenceLabels(input.Template.Labels)
	if err != nil {
		return db.Recurrence{}, fmt.Errorf("validate template_labels: %w", err)
	}
	if err := db.ValidateRecurrenceTemplate(input.Template.Title, input.Template.Metadata); err != nil {
		return db.Recurrence{}, err
	}
	firstOccurrence, err := db.ValidateRecurrenceCore(input.Rule, input.DTStart, input.Timezone)
	if err != nil {
		return db.Recurrence{}, err
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return db.Recurrence{}, fmt.Errorf("marshal recurrence labels: %w", err)
	}
	metadataJSON := json.RawMessage(`{}`)
	if len(input.Template.Metadata) > 0 {
		metadataJSON = input.Template.Metadata
	}
	recurrenceUID, err := katauid.New()
	if err != nil {
		return db.Recurrence{}, fmt.Errorf("generate recurrence uid: %w", err)
	}

	var result db.Recurrence
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, input.ProjectID))
		if err != nil {
			return err
		}
		if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, project.ID); err != nil {
			return err
		}
		var recurrenceID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO recurrences(
  uid, project_id, rrule, dtstart, timezone, template_title, template_body,
  template_owner, template_priority, template_labels, template_metadata,
  next_occurrence_key, author
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
			recurrenceUID, input.ProjectID, input.Rule, input.DTStart, input.Timezone,
			input.Template.Title, input.Template.Body, input.Template.Owner, input.Template.Priority,
			string(labelsJSON), string(metadataJSON), firstOccurrence, input.Actor,
		).Scan(&recurrenceID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			RecurrenceUID     string          `json:"recurrence_uid"`
			RRule             string          `json:"rrule"`
			DTStart           string          `json:"dtstart"`
			Timezone          string          `json:"timezone"`
			TemplateTitle     string          `json:"template_title"`
			TemplateBody      string          `json:"template_body"`
			TemplateOwner     *string         `json:"template_owner"`
			TemplatePriority  *int64          `json:"template_priority"`
			TemplateLabels    []string        `json:"template_labels"`
			TemplateMetadata  json.RawMessage `json:"template_metadata"`
			NextOccurrenceKey *string         `json:"next_occurrence_key"`
		}{
			RecurrenceUID: recurrenceUID, RRule: input.Rule, DTStart: input.DTStart,
			Timezone: input.Timezone, TemplateTitle: input.Template.Title, TemplateBody: input.Template.Body,
			TemplateOwner: input.Template.Owner, TemplatePriority: input.Template.Priority,
			TemplateLabels: labels, TemplateMetadata: metadataJSON, NextOccurrenceKey: firstOccurrence,
		})
		if err != nil {
			return fmt.Errorf("marshal recurrence.created payload: %w", err)
		}
		if _, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "recurrence.created", Actor: input.Actor, Payload: string(payload),
		}); err != nil {
			return err
		}
		result, err = scanRecurrence(tx.QueryRowContext(ctx,
			recurrenceSelect+` WHERE r.id = $1`, recurrenceID))
		return err
	})
	return result, err
}

// GetRecurrenceByID returns a recurrence, including a soft-deleted row.
func (s *Store) GetRecurrenceByID(ctx context.Context, id int64) (db.Recurrence, error) {
	return scanRecurrence(s.QueryRowContext(ctx, recurrenceSelect+` WHERE r.id = $1`, id))
}

// GetRecurrenceByUID returns a recurrence by stable identity.
func (s *Store) GetRecurrenceByUID(ctx context.Context, recurrenceUID string) (db.Recurrence, error) {
	return scanRecurrence(s.QueryRowContext(ctx, recurrenceSelect+` WHERE r.uid = $1`, recurrenceUID))
}

// ListRecurrencesByProject returns live recurrences whose project is active.
func (s *Store) ListRecurrencesByProject(ctx context.Context, projectID int64) ([]db.Recurrence, error) {
	rows, err := s.QueryContext(ctx, recurrenceSelect+`
  JOIN projects p ON p.id = r.project_id
 WHERE r.project_id = $1 AND r.deleted_at IS NULL AND p.deleted_at IS NULL
 ORDER BY r.created_at DESC, r.id DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list recurrences: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var recurrences []db.Recurrence
	for rows.Next() {
		recurrence, err := scanRecurrence(rows)
		if err != nil {
			return nil, err
		}
		recurrences = append(recurrences, recurrence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recurrences: %w", mapSQLError(err, nil))
	}
	return recurrences, nil
}

// PatchRecurrence applies an optimistic revision-guarded template update.
func (s *Store) PatchRecurrence(ctx context.Context, input db.PatchRecurrenceIn) (db.PatchRecurrenceOut, error) {
	var output db.PatchRecurrenceOut
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		current, err := scanRecurrence(tx.QueryRowContext(ctx,
			recurrenceSelect+` WHERE r.id = $1 FOR UPDATE`, input.RecurrenceID))
		if err != nil {
			return err
		}
		if current.DeletedAt != nil {
			return fmt.Errorf("recurrence %d soft-deleted", input.RecurrenceID)
		}
		if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, current.ProjectID); err != nil {
			return err
		}
		if input.IfMatchRev != current.Revision {
			return &db.RevisionConflictError{CurrentRevision: current.Revision}
		}

		next := current
		diff := make(map[string]recurrenceDiffEntry)
		addDiff := func(field string, from, to any) {
			fromJSON, _ := json.Marshal(from)
			toJSON, _ := json.Marshal(to)
			diff[field] = recurrenceDiffEntry{From: fromJSON, To: toJSON}
		}
		if value := input.Update.Rule; value != nil && *value != current.RRule {
			addDiff("rrule", current.RRule, *value)
			next.RRule = *value
		}
		if value := input.Update.DTStart; value != nil && *value != current.DTStart {
			addDiff("dtstart", current.DTStart, *value)
			next.DTStart = *value
		}
		if value := input.Update.Timezone; value != nil && *value != current.Timezone {
			addDiff("timezone", current.Timezone, *value)
			next.Timezone = *value
		}
		if value := input.Update.TemplateTitle; value != nil && *value != current.TemplateTitle {
			addDiff("template_title", current.TemplateTitle, *value)
			next.TemplateTitle = *value
		}
		if value := input.Update.TemplateBody; value != nil && *value != current.TemplateBody {
			addDiff("template_body", current.TemplateBody, *value)
			next.TemplateBody = *value
		}
		if value := input.Update.TemplateOwner; value != nil {
			nextOwner := *value
			if current.TemplateOwner == nil || *current.TemplateOwner != nextOwner {
				addDiff("template_owner", current.TemplateOwner, nextOwner)
				next.TemplateOwner = &nextOwner
			}
		}
		if value := input.Update.TemplatePriority; value != nil &&
			(current.TemplatePriority == nil || *current.TemplatePriority != *value) {
			addDiff("template_priority", current.TemplatePriority, *value)
			nextPriority := *value
			next.TemplatePriority = &nextPriority
		}
		if value := input.Update.TemplateLabels; value != nil {
			normalized, err := db.NormalizeRecurrenceLabels(*value)
			if err != nil {
				return fmt.Errorf("validate template_labels: %w", err)
			}
			labelsJSON, err := json.Marshal(normalized)
			if err != nil {
				return fmt.Errorf("marshal recurrence labels: %w", err)
			}
			if string(labelsJSON) != string(current.TemplateLabels) {
				addDiff("template_labels", json.RawMessage(current.TemplateLabels), json.RawMessage(labelsJSON))
				next.TemplateLabels = db.JSONBlob(labelsJSON)
			}
		}
		if value := input.Update.TemplateMetadata; value != nil && string(*value) != string(current.TemplateMetadata) {
			addDiff("template_metadata", json.RawMessage(current.TemplateMetadata), *value)
			next.TemplateMetadata = db.JSONBlob(*value)
		}
		if err := db.ValidateRecurrenceTemplate(next.TemplateTitle, json.RawMessage(next.TemplateMetadata)); err != nil {
			return err
		}
		if input.Update.Rule != nil || input.Update.DTStart != nil || input.Update.Timezone != nil {
			if _, err := db.ValidateRecurrenceCore(next.RRule, next.DTStart, next.Timezone); err != nil {
				return err
			}
		}
		if len(diff) == 0 {
			output = db.PatchRecurrenceOut{Recurrence: current, NewRevision: current.Revision}
			return nil
		}

		updatedAt := nowStoredTimestamp()
		newRevision := current.Revision + 1
		_, err = tx.ExecContext(ctx, `UPDATE recurrences SET
  rrule=$1, dtstart=$2, timezone=$3, template_title=$4, template_body=$5,
  template_owner=$6, template_priority=$7, template_labels=$8, template_metadata=$9,
  revision=$10, updated_at=$11 WHERE id=$12`,
			next.RRule, next.DTStart, next.Timezone, next.TemplateTitle, next.TemplateBody,
			next.TemplateOwner, next.TemplatePriority, string(next.TemplateLabels), string(next.TemplateMetadata),
			newRevision, updatedAt, current.ID,
		)
		if err != nil {
			return mapSQLError(err, nil)
		}
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1`, current.ProjectID))
		if err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			RecurrenceUID string                         `json:"recurrence_uid"`
			Diff          map[string]recurrenceDiffEntry `json:"diff"`
			RevisionNew   int64                          `json:"revision_new"`
		}{RecurrenceUID: current.UID, Diff: diff, RevisionNew: newRevision})
		if err != nil {
			return fmt.Errorf("marshal recurrence.updated payload: %w", err)
		}
		if _, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "recurrence.updated", Actor: input.Actor, Payload: string(payload),
		}); err != nil {
			return err
		}
		updated, err := scanRecurrence(tx.QueryRowContext(ctx,
			recurrenceSelect+` WHERE r.id = $1`, current.ID))
		if err != nil {
			return err
		}
		output = db.PatchRecurrenceOut{Recurrence: updated, NewRevision: newRevision, Changed: true}
		return nil
	})
	return output, err
}

// SoftDeleteRecurrence hides an active recurrence while retaining its history.
func (s *Store) SoftDeleteRecurrence(ctx context.Context, id int64, actor string) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		recurrence, err := scanRecurrence(tx.QueryRowContext(ctx,
			recurrenceSelect+` WHERE r.id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if recurrence.DeletedAt != nil {
			return fmt.Errorf("recurrence %d not found or already deleted", id)
		}
		if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, recurrence.ProjectID); err != nil {
			return err
		}
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1`, recurrence.ProjectID))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recurrences
SET deleted_at=$1, revision=revision+1, updated_at=$1 WHERE id=$2`, nowStoredTimestamp(), id); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(map[string]string{"recurrence_uid": recurrence.UID})
		if err != nil {
			return fmt.Errorf("marshal recurrence.deleted payload: %w", err)
		}
		_, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "recurrence.deleted", Actor: actor, Payload: string(payload),
		})
		return err
	})
}

// MaterializeNext creates the first recurrence occurrence strictly after the
// supplied key, or reports the already-existing occurrence.
func (s *Store) MaterializeNext(
	ctx context.Context,
	recurrenceID int64,
	afterKey string,
	actor string,
) (db.MaterializeNextOut, error) {
	var output db.MaterializeNextOut
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		output, err = s.materializeNextTx(ctx, tx, recurrenceID, afterKey, actor)
		return err
	})
	return output, err
}

func (s *Store) materializeNextTx(
	ctx context.Context,
	tx *sql.Tx,
	recurrenceID int64,
	afterKey string,
	actor string,
) (db.MaterializeNextOut, error) {
	var buffer recurrenceScanBuffer
	var projectUID, projectName string
	destinations := append(buffer.destinations(), &projectUID, &projectName)
	err := tx.QueryRowContext(ctx, `SELECT
  r.id, r.uid, r.project_id, r.rrule, r.dtstart, r.timezone,
  r.template_title, r.template_body, r.template_owner, r.template_priority,
  r.template_labels, r.template_metadata, r.next_occurrence_key,
  r.last_materialized_uid, r.author, r.revision, r.created_at, r.updated_at, r.deleted_at,
  p.uid, p.name
FROM recurrences r JOIN projects p ON p.id = r.project_id
WHERE r.id = $1 FOR UPDATE OF r`, recurrenceID).Scan(destinations...)
	if errors.Is(err, sql.ErrNoRows) {
		return db.MaterializeNextOut{}, db.ErrNotFound
	}
	if err != nil {
		return db.MaterializeNextOut{}, mapSQLError(err, nil)
	}
	current, err := buffer.value()
	if err != nil {
		return db.MaterializeNextOut{}, err
	}
	if current.DeletedAt != nil {
		return db.MaterializeNextOut{}, nil
	}
	if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, current.ProjectID); err != nil {
		return db.MaterializeNextOut{}, err
	}
	next, err := recurrence.Walk(current.RRule, current.DTStart, current.Timezone, afterKey)
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("walk rrule: %w", err)
	}
	if next == nil {
		if current.NextOccurrenceKey != nil && *current.NextOccurrenceKey != "" {
			_, err = tx.ExecContext(ctx, `UPDATE recurrences SET next_occurrence_key=NULL,
revision=revision+1, updated_at=$1 WHERE id=$2`, nowStoredTimestamp(), current.ID)
		}
		return db.MaterializeNextOut{}, mapSQLError(err, nil)
	}
	return s.materializeOccurrenceTx(ctx, tx, current, projectUID, projectName, *next, actor)
}

func (s *Store) materializeOccurrenceTx(
	ctx context.Context,
	tx *sql.Tx,
	current db.Recurrence,
	projectUID string,
	projectName string,
	occurrenceKey string,
	actor string,
) (db.MaterializeNextOut, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(current.TemplateMetadata), &metadata); err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("parse template_metadata: %w", err)
	}
	if metadata == nil {
		metadata = make(map[string]json.RawMessage)
	}
	scheduledOn, _ := json.Marshal(occurrenceKey)
	metadata["scheduled_on"] = scheduledOn
	issueMetadata, err := json.Marshal(metadata)
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("marshal issue metadata: %w", err)
	}
	issueUID, err := katauid.New()
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("generate issue uid: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, current.ProjectID); err != nil {
		return db.MaterializeNextOut{}, mapSQLError(err, nil)
	}
	shortID, err := s.resolveShortIDTx(ctx, tx, current.ProjectID, issueUID, "")
	if err != nil {
		return db.MaterializeNextOut{}, err
	}
	createdAt := nowStoredTimestamp()
	var issueID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO issues(
  uid, project_id, short_id, title, body, status, owner, priority, author,
  metadata, recurrence_id, occurrence_key, created_at, updated_at
) VALUES($1,$2,$3,$4,$5,'open',$6,$7,$8,$9,$10,$11,$12,$12)
ON CONFLICT (recurrence_id, occurrence_key)
  WHERE recurrence_id IS NOT NULL AND occurrence_key IS NOT NULL
DO NOTHING RETURNING id`,
		issueUID, current.ProjectID, shortID, current.TemplateTitle, current.TemplateBody,
		current.TemplateOwner, current.TemplatePriority, actor, string(issueMetadata),
		current.ID, occurrenceKey, createdAt,
	).Scan(&issueID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.handleMaterializeCollisionTx(ctx, tx, current, projectUID, projectName, occurrenceKey, actor)
	}
	if err != nil {
		return db.MaterializeNextOut{}, mapSQLError(err, nil)
	}
	var labels []string
	if err := json.Unmarshal([]byte(current.TemplateLabels), &labels); err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("parse template labels: %w", err)
	}
	labels, err = db.NormalizeRecurrenceLabels(labels)
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("normalize stored labels: %w", err)
	}
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_labels(issue_id, label, author) VALUES($1,$2,$3)`, issueID, label, actor,
		); err != nil {
			return db.MaterializeNextOut{}, mapSQLError(err, nil)
		}
	}
	next, err := recurrence.Walk(current.RRule, current.DTStart, current.Timezone, occurrenceKey)
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("walk after next: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recurrences SET
next_occurrence_key=$1, last_materialized_uid=$2, revision=revision+1, updated_at=$3 WHERE id=$4`,
		next, issueUID, nowStoredTimestamp(), current.ID,
	); err != nil {
		return db.MaterializeNextOut{}, mapSQLError(err, nil)
	}
	createdPayload, err := json.Marshal(issueCreatedPayload{
		UID: issueUID, ShortID: shortID, Title: current.TemplateTitle, Body: current.TemplateBody,
		Author: actor, Owner: current.TemplateOwner, Priority: current.TemplatePriority, Status: "open",
		Metadata: issueMetadata, Labels: labels, CreatedAt: createdAt,
		RecurrenceUID: current.UID, OccurrenceKey: occurrenceKey,
	})
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("marshal issue.created payload: %w", err)
	}
	if _, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: current.ProjectID, ProjectUID: projectUID, ProjectName: projectName,
		IssueID: &issueID, IssueUID: &issueUID, Type: "issue.created", Actor: actor,
		Payload: string(createdPayload),
	}); err != nil {
		return db.MaterializeNextOut{}, err
	}
	materializedPayload, err := json.Marshal(map[string]string{
		"recurrence_uid": current.UID, "occurrence_key": occurrenceKey, "issue_uid": issueUID,
	})
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("marshal recurrence.materialized payload: %w", err)
	}
	if _, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: current.ProjectID, ProjectUID: projectUID, ProjectName: projectName,
		IssueID: &issueID, IssueUID: &issueUID, Type: "recurrence.materialized", Actor: actor,
		Payload: string(materializedPayload),
	}); err != nil {
		return db.MaterializeNextOut{}, err
	}
	return db.MaterializeNextOut{
		NewIssueID: issueID, NewIssueUID: issueUID, OccurrenceKey: occurrenceKey,
	}, nil
}

func (s *Store) handleMaterializeCollisionTx(
	ctx context.Context,
	tx *sql.Tx,
	current db.Recurrence,
	projectUID string,
	projectName string,
	occurrenceKey string,
	actor string,
) (db.MaterializeNextOut, error) {
	var existingUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM issues
WHERE recurrence_id=$1 AND occurrence_key=$2`, current.ID, occurrenceKey).Scan(&existingUID); err != nil {
		return db.MaterializeNextOut{}, mapSQLError(err, nil)
	}
	next, err := recurrence.Walk(current.RRule, current.DTStart, current.Timezone, occurrenceKey)
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("walk after conflict: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recurrences SET
last_materialized_uid=$1, next_occurrence_key=$2, revision=revision+1, updated_at=$3 WHERE id=$4`,
		existingUID, next, nowStoredTimestamp(), current.ID,
	); err != nil {
		return db.MaterializeNextOut{}, mapSQLError(err, nil)
	}
	payload, err := json.Marshal(map[string]string{
		"recurrence_uid": current.UID, "occurrence_key": occurrenceKey,
		"existing_issue_uid": existingUID, "reason": "already_exists",
	})
	if err != nil {
		return db.MaterializeNextOut{}, fmt.Errorf("marshal recurrence.materialization_skipped payload: %w", err)
	}
	if _, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: current.ProjectID, ProjectUID: projectUID, ProjectName: projectName,
		Type: "recurrence.materialization_skipped", Actor: actor, Payload: string(payload),
	}); err != nil {
		return db.MaterializeNextOut{}, err
	}
	return db.MaterializeNextOut{
		NewIssueUID: existingUID, OccurrenceKey: occurrenceKey, Skipped: true,
	}, nil
}

type recurrenceScanBuffer struct {
	recurrence           db.Recurrence
	createdAt, updatedAt string
	deletedAt            sql.NullString
}

func (buffer *recurrenceScanBuffer) destinations() []any {
	return []any{
		&buffer.recurrence.ID, &buffer.recurrence.UID, &buffer.recurrence.ProjectID,
		&buffer.recurrence.RRule, &buffer.recurrence.DTStart, &buffer.recurrence.Timezone,
		&buffer.recurrence.TemplateTitle, &buffer.recurrence.TemplateBody,
		&buffer.recurrence.TemplateOwner, &buffer.recurrence.TemplatePriority,
		&buffer.recurrence.TemplateLabels, &buffer.recurrence.TemplateMetadata,
		&buffer.recurrence.NextOccurrenceKey, &buffer.recurrence.LastMaterializedUID,
		&buffer.recurrence.Author, &buffer.recurrence.Revision,
		&buffer.createdAt, &buffer.updatedAt, &buffer.deletedAt,
	}
}

func (buffer *recurrenceScanBuffer) value() (db.Recurrence, error) {
	var err error
	buffer.recurrence.CreatedAt, err = parseStoredTime(buffer.createdAt)
	if err != nil {
		return db.Recurrence{}, fmt.Errorf("parse recurrence created_at: %w", err)
	}
	buffer.recurrence.UpdatedAt, err = parseStoredTime(buffer.updatedAt)
	if err != nil {
		return db.Recurrence{}, fmt.Errorf("parse recurrence updated_at: %w", err)
	}
	if buffer.deletedAt.Valid {
		value, err := parseStoredTime(buffer.deletedAt.String)
		if err != nil {
			return db.Recurrence{}, fmt.Errorf("parse recurrence deleted_at: %w", err)
		}
		buffer.recurrence.DeletedAt = &value
	}
	return buffer.recurrence, nil
}

func scanRecurrence(row rowScanner) (db.Recurrence, error) {
	var buffer recurrenceScanBuffer
	err := row.Scan(buffer.destinations()...)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Recurrence{}, db.ErrNotFound
	}
	if err != nil {
		return db.Recurrence{}, mapSQLError(err, nil)
	}
	return buffer.value()
}
