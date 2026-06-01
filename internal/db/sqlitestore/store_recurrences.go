package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/recurrence"
	"go.kenn.io/kata/internal/shortid"
	katauid "go.kenn.io/kata/internal/uid"
	sqlite3 "modernc.org/sqlite/lib"
)

// validateRecurrenceCore checks the (rrule, dtstart, timezone) triple by
// computing the first occurrence and returns the cursor on success. Wraps
// any parse failure in ErrInvalidRecurrence.
func validateRecurrenceCore(rule, dtstart, tz string) (*string, error) {
	first, err := recurrence.Next(rule, dtstart, tz)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", db.ErrInvalidRecurrence, err)
	}
	return first, nil
}

// validateRecurrenceTemplate enforces the invariants the recurrence
// engine assumes when materializing instances: a non-empty title and a
// metadata blob that is either absent or a JSON object. Body, owner,
// priority, and labels are validated elsewhere.
func validateRecurrenceTemplate(title string, metadata json.RawMessage) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("%w: template_title must be non-empty", db.ErrInvalidRecurrence)
	}
	if len(metadata) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(metadata, &obj); err != nil {
			return fmt.Errorf("%w: template_metadata must be a JSON object: %v",
				db.ErrInvalidRecurrence, err)
		}
		// json.Unmarshal of the literal `null` into a map sets obj to nil
		// without error, slipping past the object-only invariant. Reject
		// it explicitly so MaterializeNext never sees a non-object blob.
		if obj == nil {
			return fmt.Errorf("%w: template_metadata must be a JSON object, got null",
				db.ErrInvalidRecurrence)
		}
	}
	return nil
}

// labelRe is the pattern from the schema CHECK: label must consist only of
// a-z, 0-9, '.', '_', ':', '-' and be between 1 and 64 bytes long.
// We validate this at write time so the DB constraint is never surprised.
var labelAllowedChars = func() [256]bool {
	var t [256]bool
	const allowed = "abcdefghijklmnopqrstuvwxyz0123456789._:-"
	for i := 0; i < len(allowed); i++ {
		t[allowed[i]] = true
	}
	return t
}()

// dedupeNormalizeLabels trims, lowercases, and deduplicates labels. It returns
// an error for any label that is empty after trimming, exceeds 64 bytes, or
// contains characters outside [a-z0-9._:-] (matching the schema CHECK).
// The returned slice is sorted for determinism.
func dedupeNormalizeLabels(in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		lbl := strings.TrimSpace(strings.ToLower(raw))
		if len(lbl) == 0 {
			return nil, fmt.Errorf("%w: label must be 1-64 characters", db.ErrLabelInvalid)
		}
		if len(lbl) > 64 {
			return nil, fmt.Errorf("%w: label %q must be 1-64 characters", db.ErrLabelInvalid, lbl)
		}
		for i := 0; i < len(lbl); i++ {
			if !labelAllowedChars[lbl[i]] {
				return nil, fmt.Errorf("%w: label %q contains invalid characters", db.ErrLabelInvalid, lbl)
			}
		}
		if _, dup := seen[lbl]; !dup {
			seen[lbl] = struct{}{}
			out = append(out, lbl)
		}
	}
	// Sort for deterministic storage and diffing.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// CreateRecurrence inserts a new recurrence row, emits a recurrence.created
// event, and returns the freshly-read row.
func (d *Store) CreateRecurrence(ctx context.Context, in db.CreateRecurrenceIn) (db.Recurrence, error) {
	var rec db.Recurrence
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return rec, err
	}
	defer func() { _ = tx.Rollback() }()

	// events.project_name is NOT NULL — load it before inserting the event.
	var projectName string
	if err := tx.QueryRowContext(ctx,
		`SELECT name FROM projects WHERE id = ? AND deleted_at IS NULL`,
		in.ProjectID,
	).Scan(&projectName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rec, fmt.Errorf("project %d not found", in.ProjectID)
		}
		return rec, err
	}
	if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, in.ProjectID); err != nil {
		return rec, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.ProjectID); err != nil {
		return rec, err
	}

	recUID, err := katauid.New()
	if err != nil {
		return rec, fmt.Errorf("generate recurrence uid: %w", err)
	}

	normalizedLabels, err := dedupeNormalizeLabels(in.Template.Labels)
	if err != nil {
		return rec, fmt.Errorf("validate template_labels: %w", err)
	}
	labelsJSON := "[]"
	if len(normalizedLabels) > 0 {
		b, merr := json.Marshal(normalizedLabels)
		if merr != nil {
			return rec, fmt.Errorf("marshal labels: %w", merr)
		}
		labelsJSON = string(b)
	}
	metaJSON := "{}"
	if len(in.Template.Metadata) > 0 {
		metaJSON = string(in.Template.Metadata)
	}

	if err := validateRecurrenceTemplate(in.Template.Title, in.Template.Metadata); err != nil {
		return rec, err
	}

	// Compute the first occurrence on or after dtstart. This both validates
	// the rrule/dtstart/timezone triple at create-time (a malformed input
	// can't be persisted only to fail later during materialization) and seeds
	// next_occurrence_key so a freshly-created recurrence does not read as
	// exhausted (NULL == exhausted is the cursor invariant MaterializeNext
	// relies on).
	firstNext, err := validateRecurrenceCore(in.Rule, in.DTStart, in.Timezone)
	if err != nil {
		return rec, err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone,
		   template_title, template_body, template_owner, template_priority,
		   template_labels, template_metadata, next_occurrence_key,
		   author, revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		recUID, in.ProjectID, in.Rule, in.DTStart, in.Timezone,
		in.Template.Title, in.Template.Body,
		in.Template.Owner, in.Template.Priority,
		labelsJSON, metaJSON, firstNext, in.Actor)
	if err != nil {
		return rec, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return rec, err
	}

	templateLabels := normalizedLabels
	if templateLabels == nil {
		templateLabels = []string{}
	}
	payload, err := json.Marshal(map[string]any{
		"recurrence_uid":      recUID,
		"rrule":               in.Rule,
		"dtstart":             in.DTStart,
		"timezone":            in.Timezone,
		"template_title":      in.Template.Title,
		"template_body":       in.Template.Body,
		"template_owner":      in.Template.Owner,
		"template_priority":   in.Template.Priority,
		"template_labels":     templateLabels,
		"template_metadata":   json.RawMessage(metaJSON),
		"next_occurrence_key": firstNext,
	})
	if err != nil {
		return rec, fmt.Errorf("marshal event payload: %w", err)
	}
	if _, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   in.ProjectID,
		ProjectName: projectName,
		Type:        "recurrence.created",
		Actor:       in.Actor,
		Payload:     string(payload),
	}); err != nil {
		return rec, err
	}

	if err := tx.Commit(); err != nil {
		return rec, err
	}
	return d.GetRecurrenceByID(ctx, id)
}

// PatchRecurrence runs an If-Match-guarded UPDATE comparing each supplied
// field against the current row. It builds a per-field {from, to} diff in
// JSON, emits a recurrence.updated event with that diff, and bumps revision.
// A patch where no fields change is a no-op: no event is emitted and revision
// is not bumped.
func (d *Store) PatchRecurrence(ctx context.Context, in db.PatchRecurrenceIn) (db.PatchRecurrenceOut, error) {
	var out db.PatchRecurrenceOut
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := d.getRecurrenceTx(ctx, tx, in.RecurrenceID)
	if errors.Is(err, db.ErrNotFound) {
		return out, fmt.Errorf("recurrence %d not found", in.RecurrenceID)
	}
	if err != nil {
		return out, err
	}
	if cur.DeletedAt != nil {
		return out, fmt.Errorf("recurrence %d soft-deleted", in.RecurrenceID)
	}
	if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, cur.ProjectID); err != nil {
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, cur.ProjectID); err != nil {
		return out, err
	}
	if in.IfMatchRev != cur.Revision {
		return out, &db.RevisionConflictError{CurrentRevision: cur.Revision}
	}

	// Validate any patched template fields up front. Empty titles or
	// non-object metadata would otherwise persist and break later
	// materialization.
	if in.Update.TemplateTitle != nil || in.Update.TemplateMetadata != nil {
		nextTitle := cur.TemplateTitle
		if in.Update.TemplateTitle != nil {
			nextTitle = *in.Update.TemplateTitle
		}
		var nextMeta json.RawMessage
		if in.Update.TemplateMetadata != nil {
			nextMeta = *in.Update.TemplateMetadata
		} else {
			nextMeta = json.RawMessage(cur.TemplateMetadata)
		}
		if err := validateRecurrenceTemplate(nextTitle, nextMeta); err != nil {
			return out, err
		}
	}

	// Validate the effective (rrule, dtstart, timezone) triple if any leg
	// changes. The current row was valid when written, so unchanged values
	// don't need re-checking — but the new combination might not parse
	// (e.g. dtstart format swap that the current rule can't iterate from).
	if in.Update.Rule != nil || in.Update.DTStart != nil || in.Update.Timezone != nil {
		nextRule := cur.RRule
		if in.Update.Rule != nil {
			nextRule = *in.Update.Rule
		}
		nextDTStart := cur.DTStart
		if in.Update.DTStart != nil {
			nextDTStart = *in.Update.DTStart
		}
		nextTZ := cur.Timezone
		if in.Update.Timezone != nil {
			nextTZ = *in.Update.Timezone
		}
		if _, err := validateRecurrenceCore(nextRule, nextDTStart, nextTZ); err != nil {
			return out, err
		}
	}

	type diffEntry struct {
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`
	}
	diff := map[string]diffEntry{}
	var sets []string
	var args []any

	addDiff := func(field string, from, to any) {
		fromJSON, _ := json.Marshal(from)
		toJSON, _ := json.Marshal(to)
		diff[field] = diffEntry{From: fromJSON, To: toJSON}
	}

	if in.Update.Rule != nil && *in.Update.Rule != cur.RRule {
		addDiff("rrule", cur.RRule, *in.Update.Rule)
		sets = append(sets, "rrule = ?")
		args = append(args, *in.Update.Rule)
	}
	if in.Update.DTStart != nil && *in.Update.DTStart != cur.DTStart {
		addDiff("dtstart", cur.DTStart, *in.Update.DTStart)
		sets = append(sets, "dtstart = ?")
		args = append(args, *in.Update.DTStart)
	}
	if in.Update.Timezone != nil && *in.Update.Timezone != cur.Timezone {
		addDiff("timezone", cur.Timezone, *in.Update.Timezone)
		sets = append(sets, "timezone = ?")
		args = append(args, *in.Update.Timezone)
	}
	if in.Update.TemplateTitle != nil && *in.Update.TemplateTitle != cur.TemplateTitle {
		addDiff("template_title", cur.TemplateTitle, *in.Update.TemplateTitle)
		sets = append(sets, "template_title = ?")
		args = append(args, *in.Update.TemplateTitle)
	}
	if in.Update.TemplateBody != nil && *in.Update.TemplateBody != cur.TemplateBody {
		addDiff("template_body", cur.TemplateBody, *in.Update.TemplateBody)
		sets = append(sets, "template_body = ?")
		args = append(args, *in.Update.TemplateBody)
	}
	if in.Update.TemplateOwner != nil {
		var curOwner string
		if cur.TemplateOwner != nil {
			curOwner = *cur.TemplateOwner
		}
		if *in.Update.TemplateOwner != curOwner {
			addDiff("template_owner", curOwner, *in.Update.TemplateOwner)
			sets = append(sets, "template_owner = ?")
			args = append(args, *in.Update.TemplateOwner)
		}
	}
	if in.Update.TemplatePriority != nil {
		if cur.TemplatePriority == nil || *cur.TemplatePriority != *in.Update.TemplatePriority {
			addDiff("template_priority", cur.TemplatePriority, *in.Update.TemplatePriority)
			sets = append(sets, "template_priority = ?")
			args = append(args, *in.Update.TemplatePriority)
		}
	}
	if in.Update.TemplateLabels != nil {
		normalized, nerr := dedupeNormalizeLabels(*in.Update.TemplateLabels)
		if nerr != nil {
			return out, fmt.Errorf("validate template_labels: %w", nerr)
		}
		nextLabels, merr := json.Marshal(normalized)
		if merr != nil {
			return out, fmt.Errorf("marshal labels: %w", merr)
		}
		if string(nextLabels) != string(cur.TemplateLabels) {
			addDiff("template_labels",
				json.RawMessage(cur.TemplateLabels), json.RawMessage(nextLabels))
			sets = append(sets, "template_labels = ?")
			args = append(args, string(nextLabels))
		}
	}
	if in.Update.TemplateMetadata != nil {
		if string(*in.Update.TemplateMetadata) != string(cur.TemplateMetadata) {
			addDiff("template_metadata",
				json.RawMessage(cur.TemplateMetadata), *in.Update.TemplateMetadata)
			sets = append(sets, "template_metadata = ?")
			args = append(args, string(*in.Update.TemplateMetadata))
		}
	}

	if len(diff) == 0 {
		// No-op: no changed fields — commit (nothing to write) and return unchanged.
		if err := tx.Commit(); err != nil {
			return out, err
		}
		out.Recurrence = cur
		out.NewRevision = cur.Revision
		out.Changed = false
		return out, nil
	}

	newRev := cur.Revision + 1
	sets = append(sets, "revision = ?", "updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')")
	args = append(args, newRev, in.RecurrenceID)
	// sets only contains column-name literals chosen above; user values are
	// parameterized via args. Safe to concatenate.
	q := "UPDATE recurrences SET " + strings.Join(sets, ", ") + " WHERE id = ?" // #nosec G202
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return out, err
	}

	var projectName string
	if err := tx.QueryRowContext(ctx,
		`SELECT name FROM projects WHERE id = ?`, cur.ProjectID,
	).Scan(&projectName); err != nil {
		return out, err
	}

	eventPayload, merr := json.Marshal(struct {
		RecurrenceUID string               `json:"recurrence_uid"`
		Diff          map[string]diffEntry `json:"diff"`
		RevisionNew   int64                `json:"revision_new"`
	}{cur.UID, diff, newRev})
	if merr != nil {
		return out, fmt.Errorf("marshal event payload: %w", merr)
	}

	if _, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   cur.ProjectID,
		ProjectName: projectName,
		Type:        "recurrence.updated",
		Actor:       in.Actor,
		Payload:     string(eventPayload),
	}); err != nil {
		return out, err
	}

	if err := tx.Commit(); err != nil {
		return out, err
	}
	next, err := d.GetRecurrenceByID(ctx, in.RecurrenceID)
	if err != nil {
		return out, err
	}
	out.Recurrence = next
	out.NewRevision = newRev
	out.Changed = true
	return out, nil
}

// SoftDeleteRecurrence sets deleted_at on the recurrence row and emits a
// recurrence.deleted event. Returns an error if the row is already deleted
// or does not exist.
func (d *Store) SoftDeleteRecurrence(ctx context.Context, id int64, actor string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var pid int64
	var recUID, projectName string
	if err := tx.QueryRowContext(ctx, `
		SELECT r.project_id, r.uid, p.name
		  FROM recurrences r JOIN projects p ON p.id = r.project_id
		 WHERE r.id = ? AND r.deleted_at IS NULL`,
		id,
	).Scan(&pid, &recUID, &projectName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("recurrence %d not found or already deleted", id)
		}
		return err
	}
	if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, pid); err != nil {
		return err
	}
	if err := ensureProjectWritableTx(ctx, tx, pid); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE recurrences
		   SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		       revision   = revision + 1
		 WHERE id = ?`, id,
	); err != nil {
		return err
	}

	payload, merr := json.Marshal(map[string]string{"recurrence_uid": recUID})
	if merr != nil {
		return fmt.Errorf("marshal event payload: %w", merr)
	}
	if _, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   pid,
		ProjectName: projectName,
		Type:        "recurrence.deleted",
		Actor:       actor,
		Payload:     string(payload),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// recurrenceSelectFields is the canonical SELECT column list for recurrences.
const recurrenceSelectFields = `id, uid, project_id, rrule, dtstart, timezone,
    template_title, template_body, template_owner, template_priority,
    template_labels, template_metadata, next_occurrence_key,
    last_materialized_uid, author, revision, created_at, updated_at, deleted_at`

// recurrenceSelectFieldsAliased is the same list with an "r." table alias,
// used in JOIN queries where the recurrences table is aliased as "r".
const recurrenceSelectFieldsAliased = `r.id, r.uid, r.project_id, r.rrule, r.dtstart, r.timezone,
    r.template_title, r.template_body, r.template_owner, r.template_priority,
    r.template_labels, r.template_metadata, r.next_occurrence_key,
    r.last_materialized_uid, r.author, r.revision, r.created_at, r.updated_at, r.deleted_at`

// scanner is the common interface satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanRecurrence(row scanner) (db.Recurrence, error) {
	var r db.Recurrence
	err := row.Scan(
		&r.ID, &r.UID, &r.ProjectID, &r.RRule, &r.DTStart, &r.Timezone,
		&r.TemplateTitle, &r.TemplateBody, &r.TemplateOwner, &r.TemplatePriority,
		&r.TemplateLabels, &r.TemplateMetadata, &r.NextOccurrenceKey,
		&r.LastMaterializedUID, &r.Author, &r.Revision,
		&r.CreatedAt, &r.UpdatedAt, &r.DeletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Recurrence{}, db.ErrNotFound
	}
	return r, err
}

// GetRecurrenceByID returns the recurrence with the given row id.
func (d *Store) GetRecurrenceByID(ctx context.Context, id int64) (db.Recurrence, error) {
	return scanRecurrence(d.QueryRowContext(ctx,
		"SELECT "+recurrenceSelectFields+" FROM recurrences WHERE id = ?", id))
}

func (d *Store) getRecurrenceTx(ctx context.Context, tx *sql.Tx, id int64) (db.Recurrence, error) {
	return scanRecurrence(tx.QueryRowContext(ctx,
		"SELECT "+recurrenceSelectFields+" FROM recurrences WHERE id = ?", id))
}

// GetRecurrenceByUID returns the recurrence with the given UID.
func (d *Store) GetRecurrenceByUID(ctx context.Context, recUID string) (db.Recurrence, error) {
	return scanRecurrence(d.QueryRowContext(ctx,
		"SELECT "+recurrenceSelectFields+" FROM recurrences WHERE uid = ?", recUID))
}

// ListRecurrencesByProject returns all non-deleted recurrences for projectID,
// ordered by created_at DESC. Recurrences whose parent project is soft-deleted
// (archived) are excluded alongside ordinary soft-deleted recurrence rows.
func (d *Store) ListRecurrencesByProject(ctx context.Context, projectID int64) ([]db.Recurrence, error) {
	rows, err := d.QueryContext(ctx,
		"SELECT "+recurrenceSelectFieldsAliased+
			" FROM recurrences r JOIN projects p ON p.id = r.project_id"+
			" WHERE r.project_id = ? AND r.deleted_at IS NULL AND p.deleted_at IS NULL"+
			" ORDER BY r.created_at DESC",
		projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []db.Recurrence
	for rows.Next() {
		r, err := scanRecurrence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MaterializeNext opens a transaction, materializes the recurrence's next
// instance past afterKey, and commits. CloseIssue uses the private
// materializeNextTx helper instead so the close and the materialization commit
// atomically in one transaction.
func (d *Store) MaterializeNext(
	ctx context.Context, recurrenceID int64, afterKey, actor string,
) (db.MaterializeNextOut, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.MaterializeNextOut{}, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := d.materializeNextTx(ctx, tx, recurrenceID, afterKey, actor)
	if err != nil {
		return db.MaterializeNextOut{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.MaterializeNextOut{}, err
	}
	return out, nil
}

// materializeNextTx walks the recurrence's RRULE past afterKey, inserts the next
// issue instance in the same tx (seeded from the template), and emits
// issue.created + recurrence.materialized events. If the new issue's
// (recurrence_id, occurrence_key) collides with an existing row (race with
// another writer), it emits recurrence.materialization_skipped instead and
// advances next_occurrence_key one step past the duplicate so future
// materializations don't loop on the same key. When the rule is exhausted,
// next_occurrence_key is set to NULL — consumers derive "exhausted" state
// from that NULL rather than a dedicated event.
func (d *Store) materializeNextTx(
	ctx context.Context, tx *sql.Tx, recurrenceID int64, afterKey, actor string,
) (db.MaterializeNextOut, error) {
	var out db.MaterializeNextOut

	var (
		r           db.Recurrence
		projectName string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT r.id, r.uid, r.project_id, p.name,
		       r.rrule, r.dtstart, r.timezone,
		       r.template_title, r.template_body, r.template_owner, r.template_priority,
		       r.template_labels, r.template_metadata, r.next_occurrence_key,
		       r.last_materialized_uid, r.author, r.revision,
		       r.created_at, r.updated_at, r.deleted_at
		  FROM recurrences r JOIN projects p ON p.id = r.project_id
		 WHERE r.id = ?`, recurrenceID,
	).Scan(&r.ID, &r.UID, &r.ProjectID, &projectName,
		&r.RRule, &r.DTStart, &r.Timezone,
		&r.TemplateTitle, &r.TemplateBody, &r.TemplateOwner, &r.TemplatePriority,
		&r.TemplateLabels, &r.TemplateMetadata, &r.NextOccurrenceKey,
		&r.LastMaterializedUID, &r.Author, &r.Revision,
		&r.CreatedAt, &r.UpdatedAt, &r.DeletedAt)
	if err != nil {
		return out, err
	}
	if r.DeletedAt != nil {
		return out, nil
	}
	if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, r.ProjectID); err != nil {
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, r.ProjectID); err != nil {
		return out, err
	}

	next, err := recurrence.Walk(r.RRule, r.DTStart, r.Timezone, afterKey)
	if err != nil {
		return out, fmt.Errorf("walk rrule: %w", err)
	}

	if next == nil {
		// Rule is exhausted — clear the cursor so consumers can derive
		// "exhausted" state from next_occurrence_key IS NULL. Only update when
		// the cursor was previously non-null to avoid spurious revision bumps.
		if r.NextOccurrenceKey != nil && *r.NextOccurrenceKey != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE recurrences
				    SET next_occurrence_key = NULL,
				        revision             = revision + 1,
				        updated_at           = strftime('%Y-%m-%dT%H:%M:%fZ','now')
				  WHERE id = ?`, recurrenceID,
			); err != nil {
				return out, err
			}
		}
		return out, nil
	}
	nextKey := *next

	// Compose new issue metadata: template_metadata merged with scheduled_on.
	var tmplMeta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.TemplateMetadata), &tmplMeta); err != nil {
		return out, fmt.Errorf("parse template_metadata: %w", err)
	}
	if tmplMeta == nil {
		tmplMeta = map[string]json.RawMessage{}
	}
	scheduledJSON, _ := json.Marshal(nextKey)
	tmplMeta["scheduled_on"] = scheduledJSON
	issueMetadata, err := json.Marshal(tmplMeta)
	if err != nil {
		return out, fmt.Errorf("marshal issue metadata: %w", err)
	}

	newUID, err := katauid.New()
	if err != nil {
		return out, fmt.Errorf("generate uid: %w", err)
	}
	newShortID, err := assignShortIDIn(ctx, tx, []int64{r.ProjectID}, newUID, shortid.MinLength)
	if err != nil {
		return out, fmt.Errorf("assign short_id: %w", err)
	}
	createdAt := time.Now().UTC().Format(sqliteTimeFormat)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO issues
		  (uid, project_id, short_id, title, body, status,
		   owner, priority, author, metadata, revision,
		   recurrence_id, occurrence_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		newUID, r.ProjectID, newShortID, r.TemplateTitle, r.TemplateBody,
		r.TemplateOwner, r.TemplatePriority, actor, string(issueMetadata),
		r.ID, nextKey, createdAt, createdAt,
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return d.handleMaterializeCollision(ctx, tx, r, projectName, nextKey, actor)
		}
		return out, err
	}

	var newIssueID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM issues WHERE uid = ?`, newUID,
	).Scan(&newIssueID); err != nil {
		return out, err
	}

	// Seed labels from template_labels.
	var labels []string
	_ = json.Unmarshal([]byte(r.TemplateLabels), &labels)
	// Defensive: stored template_labels may pre-date dedupe normalization
	// (e.g. imported JSONL). Normalize before insertion to avoid hitting the
	// (issue_id, label) PRIMARY KEY on the materialization tx.
	labels, err = dedupeNormalizeLabels(labels)
	if err != nil {
		return out, fmt.Errorf("normalize stored labels: %w", err)
	}
	for _, lbl := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_labels (issue_id, label, author) VALUES (?, ?, ?)`,
			newIssueID, lbl, actor,
		); err != nil {
			return out, err
		}
	}

	// Advance recurrence cursor to the key strictly after nextKey.
	afterNext, err := recurrence.Walk(r.RRule, r.DTStart, r.Timezone, nextKey)
	if err != nil {
		return out, fmt.Errorf("walk after next: %w", err)
	}
	var nextNext *string
	if afterNext != nil {
		v := *afterNext
		nextNext = &v
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE recurrences
		    SET next_occurrence_key   = ?,
		        last_materialized_uid = ?,
		        revision              = revision + 1,
		        updated_at            = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		  WHERE id = ?`,
		nextNext, newUID, r.ID,
	); err != nil {
		return out, err
	}

	// Emit issue.created (with recurrence linkage) and recurrence.materialized.
	issueCreatedPayload, err := buildIssueCreatedPayload(issueCreatedPayload{
		UID:           newUID,
		ShortID:       newShortID,
		Title:         r.TemplateTitle,
		Body:          r.TemplateBody,
		Author:        actor,
		Owner:         r.TemplateOwner,
		Priority:      r.TemplatePriority,
		Status:        "open",
		Metadata:      json.RawMessage(issueMetadata),
		Labels:        labels,
		CreatedAt:     createdAt,
		RecurrenceUID: r.UID,
		OccurrenceKey: nextKey,
	})
	if err != nil {
		return out, err
	}
	if _, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   r.ProjectID,
		ProjectName: projectName,
		IssueID:     &newIssueID,
		IssueUID:    &newUID,
		Type:        "issue.created",
		Actor:       actor,
		Payload:     string(issueCreatedPayload),
	}); err != nil {
		return out, err
	}

	matPayload, err := json.Marshal(map[string]string{
		"recurrence_uid": r.UID,
		"occurrence_key": nextKey,
		"issue_uid":      newUID,
	})
	if err != nil {
		return out, fmt.Errorf("marshal materialized payload: %w", err)
	}
	if _, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   r.ProjectID,
		ProjectName: projectName,
		IssueID:     &newIssueID,
		IssueUID:    &newUID,
		Type:        "recurrence.materialized",
		Actor:       actor,
		Payload:     string(matPayload),
	}); err != nil {
		return out, err
	}

	out.NewIssueID = newIssueID
	out.NewIssueUID = newUID
	out.OccurrenceKey = nextKey
	return out, nil
}

// handleMaterializeCollision handles the race where (recurrence_id, occurrence_key)
// already exists. It advances next_occurrence_key one step past the duplicate
// (or sets it to NULL when the rule is now exhausted) and emits
// recurrence.materialization_skipped. Returns a MaterializeNextOut with
// Skipped=true, or an error.
func (d *Store) handleMaterializeCollision(
	ctx context.Context, tx *sql.Tx, r db.Recurrence, projectName, nextKey, actor string,
) (db.MaterializeNextOut, error) {
	var out db.MaterializeNextOut

	var existingUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT uid FROM issues WHERE recurrence_id = ? AND occurrence_key = ?`,
		r.ID, nextKey,
	).Scan(&existingUID); err != nil {
		return out, err
	}

	// Advance cursor PAST nextKey so future materializations don't loop on the
	// duplicate. If afterNext is nil (exhausted), set NULL.
	afterNext, err := recurrence.Walk(r.RRule, r.DTStart, r.Timezone, nextKey)
	if err != nil {
		return out, fmt.Errorf("walk after conflict: %w", err)
	}
	var nextNext *string
	if afterNext != nil {
		v := *afterNext
		nextNext = &v
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE recurrences
		    SET last_materialized_uid = ?,
		        next_occurrence_key   = ?,
		        revision              = revision + 1,
		        updated_at            = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		  WHERE id = ?`,
		existingUID, nextNext, r.ID,
	); err != nil {
		return out, err
	}

	skipPayload, mErr := json.Marshal(map[string]string{
		"recurrence_uid":     r.UID,
		"occurrence_key":     nextKey,
		"existing_issue_uid": existingUID,
		"reason":             "already_exists",
	})
	if mErr != nil {
		return out, fmt.Errorf("marshal skipped payload: %w", mErr)
	}
	if _, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   r.ProjectID,
		ProjectName: projectName,
		Type:        "recurrence.materialization_skipped",
		Actor:       actor,
		Payload:     string(skipPayload),
	}); err != nil {
		return out, err
	}

	out.Skipped = true
	out.OccurrenceKey = nextKey
	out.NewIssueUID = existingUID
	return out, nil
}

// isUniqueConstraint reports whether err is a SQLite UNIQUE constraint violation.
func isUniqueConstraint(err error) bool {
	var coded sqliteCodeError
	if !errors.As(err, &coded) {
		return false
	}
	return coded.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
