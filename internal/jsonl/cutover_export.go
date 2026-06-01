package jsonl

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// exportForCutover writes a deterministic JSONL export of d to w. It is the
// SQLite-file-bound exporter used by cutover, which reads pre-v10 on-disk
// databases via PRAGMA introspection and version-gated SELECTs. The current-
// schema export path is jsonl.Export, which goes through db.Storage iterators
// instead.
func exportForCutover(ctx context.Context, d *sqlitestore.Store, w io.Writer, opts ExportOptions) error {
	enc := NewEncoder(w)

	version, err := schemaVersion(ctx, d)
	if err != nil {
		return err
	}
	sourceSchemaVersion, err := strconv.Atoi(version)
	if err != nil {
		return fmt.Errorf("parse schema_version %q: %w", version, err)
	}
	if err := writeRecord(enc, KindMeta, metaRecord{Key: "export_version", Value: version}); err != nil {
		return err
	}
	if err := exportMeta(ctx, d, enc); err != nil {
		return err
	}
	if err := exportProjects(ctx, d, enc, opts, sourceSchemaVersion); err != nil {
		return err
	}
	if err := exportProjectAliases(ctx, d, enc, opts); err != nil {
		return err
	}
	if sourceSchemaVersion >= 10 {
		if err := exportRecurrences(ctx, d, enc, opts); err != nil {
			return err
		}
	}
	if err := exportIssues(ctx, d, enc, opts, sourceSchemaVersion); err != nil {
		return err
	}
	if err := exportComments(ctx, d, enc, opts, sourceSchemaVersion); err != nil {
		return err
	}
	if err := exportIssueLabels(ctx, d, enc, opts); err != nil {
		return err
	}
	if err := exportLinks(ctx, d, enc, opts, sourceSchemaVersion); err != nil {
		return err
	}
	if sourceSchemaVersion >= 5 {
		if err := exportImportMappings(ctx, d, enc, opts); err != nil {
			return err
		}
	}
	if sourceSchemaVersion >= 12 {
		if err := exportFederationBindings(ctx, d, enc, opts); err != nil {
			return err
		}
	}
	if sourceSchemaVersion >= 12 {
		if err := exportFederationSyncStatus(ctx, d, enc, opts); err != nil {
			return err
		}
	}
	if sourceSchemaVersion >= 12 {
		if err := exportFederationQuarantine(ctx, d, enc, opts); err != nil {
			return err
		}
	}
	if sourceSchemaVersion >= 12 {
		if err := exportFederationEnrollments(ctx, d, enc, opts); err != nil {
			return err
		}
	}
	if sourceSchemaVersion >= 12 {
		if err := exportIssueClaims(ctx, d, enc, opts); err != nil {
			return err
		}
		if err := exportPendingClaimRequests(ctx, d, enc, opts); err != nil {
			return err
		}
	}
	if err := exportEvents(ctx, d, enc, opts, sourceSchemaVersion); err != nil {
		return err
	}
	if err := exportPurgeLog(ctx, d, enc, opts, sourceSchemaVersion); err != nil {
		return err
	}
	if err := exportSQLiteSequence(ctx, d, enc); err != nil {
		return err
	}
	return nil
}

func schemaVersion(ctx context.Context, d *sqlitestore.Store) (string, error) {
	var version string
	if err := d.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		return "", fmt.Errorf("read schema_version: %w", err)
	}
	return version, nil
}

func exportMeta(ctx context.Context, d *sqlitestore.Store, enc *Encoder) error {
	rows, err := d.QueryContext(ctx, `SELECT key, value FROM meta ORDER BY key ASC`)
	if err != nil {
		return fmt.Errorf("export meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rec metaRecord
		if err := rows.Scan(&rec.Key, &rec.Value); err != nil {
			return fmt.Errorf("scan meta: %w", err)
		}
		if err := writeRecord(enc, KindMeta, rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportProjects(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, sourceSchemaVersion int) error {
	hasIdentity, err := tableHasColumn(ctx, d, "projects", "identity")
	if err != nil {
		return err
	}
	if hasIdentity {
		if sourceSchemaVersion < 2 {
			return exportProjectsV1(ctx, d, enc, opts)
		}
		if sourceSchemaVersion < 4 {
			return exportProjectsV2(ctx, d, enc, opts)
		}
		if sourceSchemaVersion < 7 {
			return exportProjectsV4(ctx, d, enc, opts)
		}
	}
	if sourceSchemaVersion < 8 {
		return exportProjectsV7(ctx, d, enc, opts)
	}
	if sourceSchemaVersion < 10 {
		return exportProjectsV8(ctx, d, enc, opts)
	}
	type record struct {
		ID        int64           `json:"id"`
		UID       string          `json:"uid"`
		Name      string          `json:"name"`
		CreatedAt string          `json:"created_at"`
		DeletedAt *string         `json:"deleted_at,omitempty"`
		Metadata  json.RawMessage `json:"metadata"`
		Revision  int64           `json:"revision"`
	}
	query := `SELECT id, uid, name, CAST(created_at AS TEXT),
	                 CAST(deleted_at AS TEXT), metadata, revision FROM projects`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export projects: %w", err)
	}
	return scanRecords(rows, KindProject, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var metadata string
		err := rows.Scan(&rec.ID, &rec.UID, &rec.Name, &rec.CreatedAt, &rec.DeletedAt,
			&metadata, &rec.Revision)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(metadata)) {
			return rec, fmt.Errorf("project %d metadata is invalid JSON", rec.ID)
		}
		rec.Metadata = json.RawMessage(metadata)
		return rec, nil
	})
}

// exportProjectsV8 emits the v8/v9 projects projection: identity and
// next_issue_number are already gone, but metadata + revision are v10
// additions that don't exist on the source table yet. Reading them via the
// current path would fail with "no such column: metadata" on a real v9
// database, even though the v9 cutover fixture (which keeps the v10
// physical schema) does not surface this. The import path defaults
// metadata to {} and revision to 1 when those fields are absent from a
// record, so omitting them here produces correct v10 rows downstream.
func exportProjectsV8(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID        int64   `json:"id"`
		UID       string  `json:"uid"`
		Name      string  `json:"name"`
		CreatedAt string  `json:"created_at"`
		DeletedAt *string `json:"deleted_at,omitempty"`
	}
	query := `SELECT id, uid, name, CAST(created_at AS TEXT), CAST(deleted_at AS TEXT) FROM projects`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export projects: %w", err)
	}
	return scanRecords(rows, KindProject, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.Name, &rec.CreatedAt, &rec.DeletedAt)
		return rec, err
	})
}

// exportProjectsV7 emits the v7 projects projection (with next_issue_number
// and without the identity column). Used when cutting over from a v7 source.
func exportProjectsV7(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID              int64   `json:"id"`
		UID             string  `json:"uid"`
		Name            string  `json:"name"`
		CreatedAt       string  `json:"created_at"`
		NextIssueNumber int64   `json:"next_issue_number"`
		DeletedAt       *string `json:"deleted_at,omitempty"`
	}
	query := `SELECT id, uid, name, CAST(created_at AS TEXT), next_issue_number,
	                 CAST(deleted_at AS TEXT) FROM projects`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export projects: %w", err)
	}
	return scanRecords(rows, KindProject, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.Name, &rec.CreatedAt,
			&rec.NextIssueNumber, &rec.DeletedAt)
		return rec, err
	})
}

func exportProjectsV4(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID              int64   `json:"id"`
		UID             string  `json:"uid"`
		Identity        string  `json:"identity"`
		Name            string  `json:"name"`
		CreatedAt       string  `json:"created_at"`
		NextIssueNumber int64   `json:"next_issue_number"`
		DeletedAt       *string `json:"deleted_at,omitempty"`
	}
	query := `SELECT id, uid, identity, name, CAST(created_at AS TEXT), next_issue_number,
	                 CAST(deleted_at AS TEXT) FROM projects`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export projects: %w", err)
	}
	return scanRecords(rows, KindProject, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.Identity, &rec.Name, &rec.CreatedAt,
			&rec.NextIssueNumber, &rec.DeletedAt)
		return rec, err
	})
}

// exportProjectsV2 covers schema versions 2 and 3 (UID present, deleted_at
// absent). Kept distinct so cutover from v3→v4 reads the source via the
// pre-v4 column list and lets the import path default deleted_at to NULL.
func exportProjectsV2(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID              int64  `json:"id"`
		UID             string `json:"uid"`
		Identity        string `json:"identity"`
		Name            string `json:"name"`
		CreatedAt       string `json:"created_at"`
		NextIssueNumber int64  `json:"next_issue_number"`
	}
	query := `SELECT id, uid, identity, name, CAST(created_at AS TEXT), next_issue_number FROM projects`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export projects: %w", err)
	}
	return scanRecords(rows, KindProject, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.Identity, &rec.Name, &rec.CreatedAt, &rec.NextIssueNumber)
		return rec, err
	})
}

func exportProjectsV1(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID              int64  `json:"id"`
		Identity        string `json:"identity"`
		Name            string `json:"name"`
		CreatedAt       string `json:"created_at"`
		NextIssueNumber int64  `json:"next_issue_number"`
	}
	query := `SELECT id, identity, name, CAST(created_at AS TEXT), next_issue_number FROM projects`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export projects: %w", err)
	}
	return scanRecords(rows, KindProject, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.Identity, &rec.Name, &rec.CreatedAt, &rec.NextIssueNumber)
		return rec, err
	})
}

func exportProjectAliases(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID            int64  `json:"id"`
		ProjectID     int64  `json:"project_id"`
		AliasIdentity string `json:"alias_identity"`
		AliasKind     string `json:"alias_kind"`
		RootPath      string `json:"root_path"`
		CreatedAt     string `json:"created_at"`
		LastSeenAt    string `json:"last_seen_at"`
	}
	query := `SELECT id, project_id, alias_identity, alias_kind, root_path,
	                 CAST(created_at AS TEXT), CAST(last_seen_at AS TEXT)
	          FROM project_aliases`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export project_aliases: %w", err)
	}
	return scanRecords(rows, KindProjectAlias, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.AliasIdentity, &rec.AliasKind,
			&rec.RootPath, &rec.CreatedAt, &rec.LastSeenAt)
		return rec, err
	})
}

func exportRecurrences(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID                  int64           `json:"id"`
		UID                 string          `json:"uid"`
		ProjectID           int64           `json:"project_id"`
		RRule               string          `json:"rrule"`
		DTStart             string          `json:"dtstart"`
		Timezone            string          `json:"timezone"`
		TemplateTitle       string          `json:"template_title"`
		TemplateBody        string          `json:"template_body"`
		TemplateOwner       *string         `json:"template_owner,omitempty"`
		TemplatePriority    *int64          `json:"template_priority,omitempty"`
		TemplateLabels      json.RawMessage `json:"template_labels"`
		TemplateMetadata    json.RawMessage `json:"template_metadata"`
		NextOccurrenceKey   *string         `json:"next_occurrence_key,omitempty"`
		LastMaterializedUID *string         `json:"last_materialized_uid,omitempty"`
		Author              string          `json:"author"`
		Revision            int64           `json:"revision"`
		CreatedAt           string          `json:"created_at"`
		UpdatedAt           string          `json:"updated_at"`
		DeletedAt           *string         `json:"deleted_at,omitempty"`
	}
	query := `SELECT id, uid, project_id, rrule, dtstart, timezone,
	                 template_title, template_body, template_owner, template_priority,
	                 template_labels, template_metadata,
	                 next_occurrence_key, last_materialized_uid,
	                 author, revision,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(deleted_at AS TEXT)
	          FROM recurrences`
	var where []string
	var args []any
	if opts.ProjectID > 0 {
		where = append(where, "project_id = ?")
		args = append(args, opts.ProjectID)
	}
	if !opts.IncludeDeleted {
		where = append(where, `(deleted_at IS NULL
		                        OR id IN (SELECT DISTINCT recurrence_id FROM issues
		                                   WHERE recurrence_id IS NOT NULL
		                                     AND deleted_at IS NULL))`)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id ASC"

	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export recurrences: %w", err)
	}
	return scanRecords(rows, KindRecurrence, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var labels, metadata string
		err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.RRule, &rec.DTStart,
			&rec.Timezone, &rec.TemplateTitle, &rec.TemplateBody,
			&rec.TemplateOwner, &rec.TemplatePriority,
			&labels, &metadata,
			&rec.NextOccurrenceKey, &rec.LastMaterializedUID,
			&rec.Author, &rec.Revision,
			&rec.CreatedAt, &rec.UpdatedAt, &rec.DeletedAt)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(labels)) {
			return rec, fmt.Errorf("recurrence %d template_labels is invalid JSON", rec.ID)
		}
		if !json.Valid([]byte(metadata)) {
			return rec, fmt.Errorf("recurrence %d template_metadata is invalid JSON", rec.ID)
		}
		rec.TemplateLabels = json.RawMessage(labels)
		rec.TemplateMetadata = json.RawMessage(metadata)
		return rec, nil
	})
}

func exportIssues(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, sourceSchemaVersion int) error {
	if sourceSchemaVersion < 2 {
		return exportIssuesV1(ctx, d, enc, opts)
	}
	if sourceSchemaVersion < 6 {
		return exportIssuesV2(ctx, d, enc, opts)
	}
	if sourceSchemaVersion < 8 {
		return exportIssuesV6(ctx, d, enc, opts)
	}
	if sourceSchemaVersion < 10 {
		return exportIssuesV8(ctx, d, enc, opts)
	}
	type record struct {
		ID            int64           `json:"id"`
		UID           string          `json:"uid"`
		ProjectID     int64           `json:"project_id"`
		ShortID       string          `json:"short_id"`
		Title         string          `json:"title"`
		Body          string          `json:"body"`
		Status        string          `json:"status"`
		ClosedReason  *string         `json:"closed_reason"`
		Owner         *string         `json:"owner"`
		Priority      *int64          `json:"priority,omitempty"`
		Author        string          `json:"author"`
		CreatedAt     string          `json:"created_at"`
		UpdatedAt     string          `json:"updated_at"`
		ClosedAt      *string         `json:"closed_at"`
		DeletedAt     *string         `json:"deleted_at"`
		Metadata      json.RawMessage `json:"metadata"`
		Revision      int64           `json:"revision"`
		RecurrenceID  *int64          `json:"recurrence_id,omitempty"`
		RecurrenceUID *string         `json:"recurrence_uid,omitempty"`
		OccurrenceKey *string         `json:"occurrence_key,omitempty"`
	}
	query := `SELECT i.id, i.uid, i.project_id, i.short_id, i.title, i.body,
	                 i.status, i.closed_reason, i.owner, i.priority, i.author,
	                 CAST(i.created_at AS TEXT), CAST(i.updated_at AS TEXT),
	                 CAST(i.closed_at AS TEXT), CAST(i.deleted_at AS TEXT),
	                 i.metadata, i.revision,
	                 i.recurrence_id, r.uid, i.occurrence_key
	          FROM issues i
	          LEFT JOIN recurrences r ON r.id = i.recurrence_id`
	where, args := issueExportWhere("i", opts)
	query += where + ` ORDER BY i.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export issues: %w", err)
	}
	return scanRecords(rows, KindIssue, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var metadata string
		err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.ShortID, &rec.Title, &rec.Body,
			&rec.Status, &rec.ClosedReason, &rec.Owner, &rec.Priority, &rec.Author, &rec.CreatedAt,
			&rec.UpdatedAt, &rec.ClosedAt, &rec.DeletedAt, &metadata, &rec.Revision,
			&rec.RecurrenceID, &rec.RecurrenceUID, &rec.OccurrenceKey)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(metadata)) {
			return rec, fmt.Errorf("issue %d metadata is invalid JSON", rec.ID)
		}
		rec.Metadata = json.RawMessage(metadata)
		return rec, nil
	})
}

// exportIssuesV8 emits the schema_version 8..9 issue projection. Both
// versions predate the v10 additions: issues.metadata, issues.revision,
// issues.recurrence_id, and issues.occurrence_key — none of these columns
// exist on a real v8/v9 source DB. The import path defaults Metadata to
// {} and Revision to 1 when those fields are absent from a record, so
// omitting them here lets v9 rows replay cleanly into the v10 schema.
func exportIssuesV8(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID           int64   `json:"id"`
		UID          string  `json:"uid"`
		ProjectID    int64   `json:"project_id"`
		ShortID      string  `json:"short_id"`
		Title        string  `json:"title"`
		Body         string  `json:"body"`
		Status       string  `json:"status"`
		ClosedReason *string `json:"closed_reason"`
		Owner        *string `json:"owner"`
		Priority     *int64  `json:"priority,omitempty"`
		Author       string  `json:"author"`
		CreatedAt    string  `json:"created_at"`
		UpdatedAt    string  `json:"updated_at"`
		ClosedAt     *string `json:"closed_at"`
		DeletedAt    *string `json:"deleted_at"`
	}
	query := `SELECT id, uid, project_id, short_id, title, body, status, closed_reason, owner, priority, author,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(closed_at AS TEXT), CAST(deleted_at AS TEXT)
	          FROM issues`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export issues: %w", err)
	}
	return scanRecords(rows, KindIssue, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.ShortID, &rec.Title, &rec.Body,
			&rec.Status, &rec.ClosedReason, &rec.Owner, &rec.Priority, &rec.Author, &rec.CreatedAt,
			&rec.UpdatedAt, &rec.ClosedAt, &rec.DeletedAt)
		return rec, err
	})
}

// exportIssuesV6 emits the schema_version 6..7 issue projection (with priority
// and the now-dropped number column). Cutover from a pre-short_id source DB
// lands here; targets at v8+ derive short_ids during replay (Task 9).
func exportIssuesV6(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID           int64   `json:"id"`
		UID          string  `json:"uid"`
		ProjectID    int64   `json:"project_id"`
		Number       int64   `json:"number"`
		Title        string  `json:"title"`
		Body         string  `json:"body"`
		Status       string  `json:"status"`
		ClosedReason *string `json:"closed_reason"`
		Owner        *string `json:"owner"`
		Priority     *int64  `json:"priority,omitempty"`
		Author       string  `json:"author"`
		CreatedAt    string  `json:"created_at"`
		UpdatedAt    string  `json:"updated_at"`
		ClosedAt     *string `json:"closed_at"`
		DeletedAt    *string `json:"deleted_at"`
	}
	query := `SELECT id, uid, project_id, number, title, body, status, closed_reason, owner, priority, author,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(closed_at AS TEXT), CAST(deleted_at AS TEXT)
	          FROM issues`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export issues: %w", err)
	}
	return scanRecords(rows, KindIssue, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.Number, &rec.Title, &rec.Body,
			&rec.Status, &rec.ClosedReason, &rec.Owner, &rec.Priority, &rec.Author, &rec.CreatedAt,
			&rec.UpdatedAt, &rec.ClosedAt, &rec.DeletedAt)
		return rec, err
	})
}

// exportIssuesV2 emits the schema_version 2..5 issue projection (no priority
// column). Cutover from a pre-priority source DB lands here; targets at v6+
// silently default priority to NULL on import.
func exportIssuesV2(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID           int64   `json:"id"`
		UID          string  `json:"uid"`
		ProjectID    int64   `json:"project_id"`
		Number       int64   `json:"number"`
		Title        string  `json:"title"`
		Body         string  `json:"body"`
		Status       string  `json:"status"`
		ClosedReason *string `json:"closed_reason"`
		Owner        *string `json:"owner"`
		Author       string  `json:"author"`
		CreatedAt    string  `json:"created_at"`
		UpdatedAt    string  `json:"updated_at"`
		ClosedAt     *string `json:"closed_at"`
		DeletedAt    *string `json:"deleted_at"`
	}
	query := `SELECT id, uid, project_id, number, title, body, status, closed_reason, owner, author,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(closed_at AS TEXT), CAST(deleted_at AS TEXT)
	          FROM issues`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export issues: %w", err)
	}
	return scanRecords(rows, KindIssue, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.Number, &rec.Title, &rec.Body,
			&rec.Status, &rec.ClosedReason, &rec.Owner, &rec.Author, &rec.CreatedAt,
			&rec.UpdatedAt, &rec.ClosedAt, &rec.DeletedAt)
		return rec, err
	})
}

func exportIssuesV1(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID           int64   `json:"id"`
		ProjectID    int64   `json:"project_id"`
		Number       int64   `json:"number"`
		Title        string  `json:"title"`
		Body         string  `json:"body"`
		Status       string  `json:"status"`
		ClosedReason *string `json:"closed_reason"`
		Owner        *string `json:"owner"`
		Author       string  `json:"author"`
		CreatedAt    string  `json:"created_at"`
		UpdatedAt    string  `json:"updated_at"`
		ClosedAt     *string `json:"closed_at"`
		DeletedAt    *string `json:"deleted_at"`
	}
	query := `SELECT id, project_id, number, title, body, status, closed_reason, owner, author,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(closed_at AS TEXT), CAST(deleted_at AS TEXT)
	          FROM issues`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export issues: %w", err)
	}
	return scanRecords(rows, KindIssue, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.Number, &rec.Title, &rec.Body,
			&rec.Status, &rec.ClosedReason, &rec.Owner, &rec.Author, &rec.CreatedAt,
			&rec.UpdatedAt, &rec.ClosedAt, &rec.DeletedAt)
		return rec, err
	})
}

func exportComments(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, sourceSchemaVersion int) error {
	if sourceSchemaVersion < 12 {
		return exportCommentsV10(ctx, d, enc, opts)
	}
	type record struct {
		ID        int64  `json:"id"`
		UID       string `json:"uid"`
		IssueID   int64  `json:"issue_id"`
		Author    string `json:"author"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	query := `SELECT comments.id, comments.uid, comments.issue_id, comments.author, comments.body, CAST(comments.created_at AS TEXT)
	          FROM comments
	          JOIN issues ON issues.id = comments.issue_id`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY comments.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export comments: %w", err)
	}
	return scanRecords(rows, KindComment, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.IssueID, &rec.Author, &rec.Body, &rec.CreatedAt)
		return rec, err
	})
}

func exportCommentsV10(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID        int64  `json:"id"`
		IssueID   int64  `json:"issue_id"`
		Author    string `json:"author"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	query := `SELECT comments.id, comments.issue_id, comments.author, comments.body, CAST(comments.created_at AS TEXT)
	          FROM comments
	          JOIN issues ON issues.id = comments.issue_id`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY comments.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export comments: %w", err)
	}
	return scanRecords(rows, KindComment, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.IssueID, &rec.Author, &rec.Body, &rec.CreatedAt)
		return rec, err
	})
}

func exportIssueLabels(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		IssueID   int64  `json:"issue_id"`
		Label     string `json:"label"`
		Author    string `json:"author"`
		CreatedAt string `json:"created_at"`
	}
	query := `SELECT issue_labels.issue_id, issue_labels.label, issue_labels.author, CAST(issue_labels.created_at AS TEXT)
	          FROM issue_labels
	          JOIN issues ON issues.id = issue_labels.issue_id`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY issue_labels.issue_id ASC, issue_labels.label ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export issue_labels: %w", err)
	}
	return scanRecords(rows, KindIssueLabel, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.IssueID, &rec.Label, &rec.Author, &rec.CreatedAt)
		return rec, err
	})
}

func exportLinks(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, sourceSchemaVersion int) error {
	if sourceSchemaVersion < 2 {
		return exportLinksV1(ctx, d, enc, opts)
	}
	type record struct {
		ID           int64  `json:"id"`
		ProjectID    int64  `json:"project_id"`
		FromIssueID  int64  `json:"from_issue_id"`
		FromIssueUID string `json:"from_issue_uid"`
		ToIssueID    int64  `json:"to_issue_id"`
		ToIssueUID   string `json:"to_issue_uid"`
		Type         string `json:"type"`
		Author       string `json:"author"`
		CreatedAt    string `json:"created_at"`
	}
	query := `SELECT links.id, links.project_id, links.from_issue_id, links.from_issue_uid,
	                 links.to_issue_id, links.to_issue_uid,
	                 links.type, links.author, CAST(links.created_at AS TEXT)
	          FROM links
	          JOIN issues AS from_issues ON from_issues.id = links.from_issue_id
	          JOIN issues AS to_issues ON to_issues.id = links.to_issue_id`
	where, args := linkExportWhere(opts)
	query += where + ` ORDER BY links.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export links: %w", err)
	}
	return scanRecords(rows, KindLink, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.FromIssueID, &rec.FromIssueUID,
			&rec.ToIssueID, &rec.ToIssueUID, &rec.Type, &rec.Author, &rec.CreatedAt)
		return rec, err
	})
}

func exportLinksV1(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID          int64  `json:"id"`
		ProjectID   int64  `json:"project_id"`
		FromIssueID int64  `json:"from_issue_id"`
		ToIssueID   int64  `json:"to_issue_id"`
		Type        string `json:"type"`
		Author      string `json:"author"`
		CreatedAt   string `json:"created_at"`
	}
	query := `SELECT links.id, links.project_id, links.from_issue_id, links.to_issue_id,
	                 links.type, links.author, CAST(links.created_at AS TEXT)
	          FROM links
	          JOIN issues AS from_issues ON from_issues.id = links.from_issue_id
	          JOIN issues AS to_issues ON to_issues.id = links.to_issue_id`
	where, args := linkExportWhere(opts)
	query += where + ` ORDER BY links.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export links: %w", err)
	}
	return scanRecords(rows, KindLink, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.FromIssueID, &rec.ToIssueID,
			&rec.Type, &rec.Author, &rec.CreatedAt)
		return rec, err
	})
}

func exportImportMappings(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID              int64   `json:"id"`
		Source          string  `json:"source"`
		ExternalID      string  `json:"external_id"`
		ObjectType      string  `json:"object_type"`
		ProjectID       int64   `json:"project_id"`
		IssueID         *int64  `json:"issue_id,omitempty"`
		CommentID       *int64  `json:"comment_id,omitempty"`
		LinkID          *int64  `json:"link_id,omitempty"`
		Label           *string `json:"label,omitempty"`
		SourceUpdatedAt *string `json:"source_updated_at,omitempty"`
		ImportedAt      string  `json:"imported_at"`
	}
	query := `SELECT id, source, external_id, object_type, project_id, issue_id, comment_id, link_id, label,
	                 CAST(source_updated_at AS TEXT), CAST(imported_at AS TEXT)
	          FROM import_mappings`
	clauses := []string{}
	args := []any{}
	if opts.ProjectID > 0 {
		clauses = append(clauses, `project_id = ?`)
		args = append(args, opts.ProjectID)
	}
	if !opts.IncludeDeleted {
		clauses = append(clauses,
			`(object_type NOT IN ('issue', 'comment', 'label') OR EXISTS (SELECT 1 FROM issues WHERE issues.id = import_mappings.issue_id AND issues.deleted_at IS NULL))`,
			`(object_type != 'link' OR EXISTS (
				SELECT 1
				FROM links
				JOIN issues AS from_issues ON from_issues.id = links.from_issue_id
				JOIN issues AS to_issues ON to_issues.id = links.to_issue_id
				WHERE links.id = import_mappings.link_id
				  AND from_issues.deleted_at IS NULL
				  AND to_issues.deleted_at IS NULL
			))`,
		)
	}
	query += whereClause(clauses) + ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export import_mappings: %w", err)
	}
	return scanRecords(rows, KindImportMapping, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.Source, &rec.ExternalID, &rec.ObjectType, &rec.ProjectID,
			&rec.IssueID, &rec.CommentID, &rec.LinkID, &rec.Label, &rec.SourceUpdatedAt, &rec.ImportedAt)
		return rec, err
	})
}

func exportFederationBindings(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ProjectID            int64   `json:"project_id"`
		Role                 string  `json:"role"`
		HubURL               string  `json:"hub_url"`
		HubProjectID         int64   `json:"hub_project_id"`
		HubProjectUID        string  `json:"hub_project_uid"`
		ReplayHorizonEventID int64   `json:"replay_horizon_event_id"`
		PullCursorEventID    int64   `json:"pull_cursor_event_id"`
		PushEnabled          bool    `json:"push_enabled"`
		PushCursorEventID    int64   `json:"push_cursor_event_id"`
		Enabled              bool    `json:"enabled"`
		CreatedAt            string  `json:"created_at"`
		UpdatedAt            string  `json:"updated_at"`
		LastSyncAt           *string `json:"last_sync_at,omitempty"`
	}
	query := `SELECT project_id, role, hub_url, hub_project_id, hub_project_uid,
	                 replay_horizon_event_id, pull_cursor_event_id, push_enabled,
	                 push_cursor_event_id, enabled,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(last_sync_at AS TEXT)
	          FROM federation_bindings`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY project_id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export federation_bindings: %w", err)
	}
	return scanRecords(rows, KindFederationBinding, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var enabled, pushEnabled int
		err := rows.Scan(&rec.ProjectID, &rec.Role, &rec.HubURL, &rec.HubProjectID,
			&rec.HubProjectUID, &rec.ReplayHorizonEventID, &rec.PullCursorEventID,
			&pushEnabled, &rec.PushCursorEventID, &enabled, &rec.CreatedAt,
			&rec.UpdatedAt, &rec.LastSyncAt)
		rec.PushEnabled = pushEnabled == 1
		rec.Enabled = enabled == 1
		return rec, err
	})
}

func exportFederationSyncStatus(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ProjectID         int64   `json:"project_id"`
		LastPullStartedAt *string `json:"last_pull_started_at,omitempty"`
		LastPullSuccessAt *string `json:"last_pull_success_at,omitempty"`
		LastPushStartedAt *string `json:"last_push_started_at,omitempty"`
		LastPushSuccessAt *string `json:"last_push_success_at,omitempty"`
		LastErrorAt       *string `json:"last_error_at,omitempty"`
		LastError         *string `json:"last_error,omitempty"`
		LastResetAt       *string `json:"last_reset_at,omitempty"`
	}
	query := `SELECT project_id,
	                 CAST(last_pull_started_at AS TEXT), CAST(last_pull_success_at AS TEXT),
	                 CAST(last_push_started_at AS TEXT), CAST(last_push_success_at AS TEXT),
	                 CAST(last_error_at AS TEXT), last_error, CAST(last_reset_at AS TEXT)
	            FROM federation_sync_status`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY project_id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export federation_sync_status: %w", err)
	}
	return scanRecords(rows, KindFederationSyncStatus, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ProjectID, &rec.LastPullStartedAt, &rec.LastPullSuccessAt,
			&rec.LastPushStartedAt, &rec.LastPushSuccessAt, &rec.LastErrorAt,
			&rec.LastError, &rec.LastResetAt)
		return rec, err
	})
}

func exportFederationQuarantine(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID           int64           `json:"id"`
		ProjectID    int64           `json:"project_id"`
		Direction    string          `json:"direction"`
		FirstEventID int64           `json:"first_event_id"`
		LastEventID  int64           `json:"last_event_id"`
		EventUIDs    json.RawMessage `json:"event_uids"`
		Error        string          `json:"error"`
		CreatedAt    string          `json:"created_at"`
		SkippedAt    *string         `json:"skipped_at,omitempty"`
		SkippedBy    *string         `json:"skipped_by,omitempty"`
		SkipReason   *string         `json:"skip_reason,omitempty"`
	}
	query := `SELECT id, project_id, direction, first_event_id, last_event_id,
	                 event_uids, error, CAST(created_at AS TEXT),
	                 CAST(skipped_at AS TEXT), skipped_by, skip_reason
	            FROM federation_quarantine`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export federation_quarantine: %w", err)
	}
	return scanRecords(rows, KindFederationQuarantine, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var eventUIDs string
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.Direction, &rec.FirstEventID,
			&rec.LastEventID, &eventUIDs, &rec.Error, &rec.CreatedAt,
			&rec.SkippedAt, &rec.SkippedBy, &rec.SkipReason)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(eventUIDs)) {
			return rec, fmt.Errorf("federation quarantine %d event_uids is invalid JSON", rec.ID)
		}
		rec.EventUIDs = json.RawMessage(eventUIDs)
		return rec, nil
	})
}

func exportFederationEnrollments(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID               int64   `json:"id"`
		TokenHash        string  `json:"token_hash"`
		SpokeInstanceUID string  `json:"spoke_instance_uid"`
		ProjectID        *int64  `json:"project_id,omitempty"`
		Capabilities     string  `json:"capabilities"`
		CreatedAt        string  `json:"created_at"`
		UpdatedAt        string  `json:"updated_at"`
		RevokedAt        *string `json:"revoked_at,omitempty"`
	}
	query := `SELECT id, token_hash, spoke_instance_uid, project_id, capabilities,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT), CAST(revoked_at AS TEXT)
	          FROM federation_enrollments`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export federation_enrollments: %w", err)
	}
	return scanRecords(rows, KindFederationEnrollment, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.TokenHash, &rec.SpokeInstanceUID, &rec.ProjectID,
			&rec.Capabilities, &rec.CreatedAt, &rec.UpdatedAt, &rec.RevokedAt)
		return rec, err
	})
}

func exportIssueClaims(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID                int64   `json:"id"`
		ClaimUID          string  `json:"claim_uid"`
		ProjectID         int64   `json:"project_id"`
		IssueID           int64   `json:"issue_id"`
		IssueUID          string  `json:"issue_uid"`
		Holder            string  `json:"holder"`
		HolderInstanceUID string  `json:"holder_instance_uid"`
		ClientKind        string  `json:"client_kind"`
		Purpose           string  `json:"purpose"`
		ClaimKind         string  `json:"claim_kind"`
		AcquiredAt        string  `json:"acquired_at"`
		ExpiresAt         *string `json:"expires_at,omitempty"`
		ReleasedAt        *string `json:"released_at,omitempty"`
		ReleaseReason     *string `json:"release_reason,omitempty"`
		Revision          int64   `json:"revision"`
		UpdatedAt         string  `json:"updated_at"`
	}
	query := `SELECT issue_claims.id, issue_claims.claim_uid, issue_claims.project_id,
	                 issue_claims.issue_id, issue_claims.issue_uid, issue_claims.holder,
	                 issue_claims.holder_instance_uid, issue_claims.client_kind,
	                 issue_claims.purpose, issue_claims.claim_kind,
	                 CAST(issue_claims.acquired_at AS TEXT),
	                 CAST(issue_claims.expires_at AS TEXT),
	                 CAST(issue_claims.released_at AS TEXT),
	                 issue_claims.release_reason, issue_claims.revision,
	                 CAST(issue_claims.updated_at AS TEXT)
	          FROM issue_claims
	          JOIN issues ON issues.id = issue_claims.issue_id`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY issue_claims.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export issue_claims: %w", err)
	}
	return scanRecords(rows, KindIssueClaim, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.ClaimUID, &rec.ProjectID, &rec.IssueID,
			&rec.IssueUID, &rec.Holder, &rec.HolderInstanceUID, &rec.ClientKind,
			&rec.Purpose, &rec.ClaimKind, &rec.AcquiredAt, &rec.ExpiresAt,
			&rec.ReleasedAt, &rec.ReleaseReason, &rec.Revision, &rec.UpdatedAt)
		return rec, err
	})
}

func exportPendingClaimRequests(
	ctx context.Context,
	d *sqlitestore.Store,
	enc *Encoder,
	opts ExportOptions,
) error {
	type record struct {
		ID                int64   `json:"id"`
		RequestUID        string  `json:"request_uid"`
		ProjectID         int64   `json:"project_id"`
		IssueID           int64   `json:"issue_id"`
		IssueUID          string  `json:"issue_uid"`
		Holder            string  `json:"holder"`
		HolderInstanceUID string  `json:"holder_instance_uid"`
		ClientKind        string  `json:"client_kind"`
		ClaimKind         string  `json:"claim_kind"`
		TTLSeconds        *int64  `json:"ttl_seconds,omitempty"`
		Purpose           string  `json:"purpose"`
		RequestedAt       string  `json:"requested_at"`
		LastAttemptAt     *string `json:"last_attempt_at,omitempty"`
		LastError         *string `json:"last_error,omitempty"`
		RejectedAt        *string `json:"rejected_at,omitempty"`
		ResolvedAt        *string `json:"resolved_at,omitempty"`
	}
	query := `SELECT pending_claim_requests.id, pending_claim_requests.request_uid,
	                 pending_claim_requests.project_id, pending_claim_requests.issue_id,
	                 pending_claim_requests.issue_uid, pending_claim_requests.holder,
	                 pending_claim_requests.holder_instance_uid, pending_claim_requests.client_kind,
	                 pending_claim_requests.claim_kind, pending_claim_requests.ttl_seconds,
	                 pending_claim_requests.purpose,
	                 CAST(pending_claim_requests.requested_at AS TEXT),
	                 CAST(pending_claim_requests.last_attempt_at AS TEXT),
	                 pending_claim_requests.last_error,
	                 CAST(pending_claim_requests.rejected_at AS TEXT),
	                 CAST(pending_claim_requests.resolved_at AS TEXT)
	          FROM pending_claim_requests
	          JOIN issues ON issues.id = pending_claim_requests.issue_id`
	where, args := issueExportWhere("issues", opts)
	query += where + ` ORDER BY pending_claim_requests.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export pending_claim_requests: %w", err)
	}
	return scanRecords(rows, KindPendingClaimRequest, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.RequestUID, &rec.ProjectID, &rec.IssueID,
			&rec.IssueUID, &rec.Holder, &rec.HolderInstanceUID, &rec.ClientKind,
			&rec.ClaimKind, &rec.TTLSeconds, &rec.Purpose, &rec.RequestedAt, &rec.LastAttemptAt, &rec.LastError,
			&rec.RejectedAt, &rec.ResolvedAt)
		return rec, err
	})
}

func exportEvents(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, sourceSchemaVersion int) error {
	projectNameExpr, joinProjects := eventProjectNameExpr(sourceSchemaVersion)
	if sourceSchemaVersion < 2 {
		return exportEventsV1(ctx, d, enc, opts, projectNameExpr, joinProjects)
	}
	if sourceSchemaVersion < 3 {
		return exportEventsV2(ctx, d, enc, opts, projectNameExpr, joinProjects)
	}
	if sourceSchemaVersion < 8 {
		return exportEventsV3(ctx, d, enc, opts, projectNameExpr, joinProjects)
	}
	if sourceSchemaVersion < 12 {
		return exportEventsV8(ctx, d, enc, opts, projectNameExpr, joinProjects)
	}
	type record struct {
		ID                int64           `json:"id"`
		UID               string          `json:"uid"`
		OriginInstanceUID string          `json:"origin_instance_uid"`
		ProjectID         int64           `json:"project_id"`
		ProjectUID        string          `json:"-"`
		ProjectName       string          `json:"project_name"`
		IssueID           *int64          `json:"issue_id"`
		IssueUID          *string         `json:"issue_uid"`
		RelatedIssueID    *int64          `json:"related_issue_id"`
		RelatedIssueUID   *string         `json:"related_issue_uid"`
		Type              string          `json:"type"`
		Actor             string          `json:"actor"`
		Payload           json.RawMessage `json:"payload"`
		HLCPhysicalMS     int64           `json:"hlc_physical_ms"`
		HLCCounter        int64           `json:"hlc_counter"`
		ContentHash       string          `json:"content_hash"`
		CreatedAt         string          `json:"created_at"`
	}
	// See exportEventsV2 for the kata#1 design call: live-only export
	// keeps aggregated issue.links_changed events whose related_issue_id
	// points at a now-soft-deleted peer, but the peer's row is omitted
	// from the export — so the FK must be scrubbed at emit time so the
	// JSONL round-trips. The payload's *_uids slices retain the orphan
	// reference for historical context.
	//
	// Scrub related_issue_id when the peer is missing entirely
	// (any event type) OR, on live-only export, when an
	// issue.links_changed peer is soft-deleted (kata#1 history-
	// preservation rule). Peer-missing must be checked first so
	// `peer.deleted_at` doesn't dereference a NULL row.
	scrubCondition := `(peer.id IS NULL AND events.related_issue_id IS NOT NULL)`
	if !opts.IncludeDeleted {
		scrubCondition += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
	}
	relatedIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_id END`
	relatedUIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_uid END`
	subjectLiveClause := `((events.issue_id IS NULL AND events.issue_uid IS NULL) OR subject_issue.id IS NOT NULL)`
	if !opts.IncludeDeleted {
		subjectLiveClause = `((events.issue_id IS NULL AND events.issue_uid IS NULL) OR (subject_issue.id IS NOT NULL AND subject_issue.deleted_at IS NULL))`
	}
	query := fmt.Sprintf(`SELECT events.id, events.uid, events.origin_instance_uid, events.project_id, export_project.uid, %s, events.issue_id, events.issue_uid,
	                 `+relatedIDExpr+`, `+relatedUIDExpr+`,
	                 events.type, events.actor, events.payload, events.hlc_physical_ms, events.hlc_counter, events.content_hash,
	                 CAST(events.created_at AS TEXT)
	          FROM events%s
	          JOIN projects export_project ON export_project.id = events.project_id
	          LEFT JOIN issues subject_issue ON subject_issue.project_id = events.project_id
	               AND (subject_issue.id = events.issue_id OR (events.issue_id IS NULL AND events.issue_uid IS NOT NULL AND subject_issue.uid = events.issue_uid))
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id`, projectNameExpr, joinProjects)
	clauses, args := eventExportWhereClauses(opts)
	clauses = append([]string{subjectLiveClause}, clauses...)
	query += whereClause(clauses) + ` ORDER BY events.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export events: %w", err)
	}
	return scanRecords(rows, KindEvent, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var payload string
		err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.ProjectUID, &rec.ProjectName, &rec.IssueID,
			&rec.IssueUID, &rec.RelatedIssueID, &rec.RelatedIssueUID,
			&rec.Type, &rec.Actor, &payload, &rec.HLCPhysicalMS, &rec.HLCCounter, &rec.ContentHash, &rec.CreatedAt)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(payload)) {
			return rec, fmt.Errorf("event %d payload is invalid JSON", rec.ID)
		}
		rec.Payload = json.RawMessage(payload)
		contentHash, err := db.EventContentHash(db.EventHashInput{
			UID:               rec.UID,
			OriginInstanceUID: rec.OriginInstanceUID,
			ProjectUID:        rec.ProjectUID,
			ProjectName:       rec.ProjectName,
			IssueUID:          rec.IssueUID,
			RelatedIssueUID:   rec.RelatedIssueUID,
			Type:              rec.Type,
			Actor:             rec.Actor,
			HLCPhysicalMS:     rec.HLCPhysicalMS,
			HLCCounter:        rec.HLCCounter,
			CreatedAt:         rec.CreatedAt,
			Payload:           rec.Payload,
		})
		if err != nil {
			return rec, fmt.Errorf("event %d content hash: %w", rec.ID, err)
		}
		rec.ContentHash = contentHash
		return rec, nil
	})
}

func exportEventsV8(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, projectNameExpr, joinProjects string) error {
	type record struct {
		ID                int64           `json:"id"`
		UID               string          `json:"uid"`
		OriginInstanceUID string          `json:"origin_instance_uid"`
		ProjectID         int64           `json:"project_id"`
		ProjectName       string          `json:"project_name"`
		IssueID           *int64          `json:"issue_id"`
		IssueUID          *string         `json:"issue_uid"`
		RelatedIssueID    *int64          `json:"related_issue_id"`
		RelatedIssueUID   *string         `json:"related_issue_uid"`
		Type              string          `json:"type"`
		Actor             string          `json:"actor"`
		Payload           json.RawMessage `json:"payload"`
		CreatedAt         string          `json:"created_at"`
	}
	scrubCondition := `(peer.id IS NULL AND events.related_issue_id IS NOT NULL)`
	if !opts.IncludeDeleted {
		scrubCondition += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
	}
	relatedIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_id END`
	relatedUIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_uid END`
	query := fmt.Sprintf(`SELECT events.id, events.uid, events.origin_instance_uid, events.project_id, %s, events.issue_id, events.issue_uid,
	                 `+relatedIDExpr+`, `+relatedUIDExpr+`,
	                 events.type, events.actor, events.payload, CAST(events.created_at AS TEXT)
	          FROM events%s
	          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id`, projectNameExpr, joinProjects)
	clauses, args := eventExportWhereClauses(opts)
	clauses = append([]string{`(events.issue_id IS NULL OR subject_issue.id IS NOT NULL)`}, clauses...)
	query += whereClause(clauses) + ` ORDER BY events.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export events: %w", err)
	}
	return scanRecords(rows, KindEvent, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var payload string
		err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.ProjectName, &rec.IssueID,
			&rec.IssueUID, &rec.RelatedIssueID, &rec.RelatedIssueUID,
			&rec.Type, &rec.Actor, &payload, &rec.CreatedAt)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(payload)) {
			return rec, fmt.Errorf("event %d payload is invalid JSON", rec.ID)
		}
		rec.Payload = json.RawMessage(payload)
		return rec, nil
	})
}

// exportEventsV3 emits the v3..v7 events projection (with the now-dropped
// issue_number column). Cutover from a pre-short_id source DB lands here.
func exportEventsV3(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, projectNameExpr, joinProjects string) error {
	type record struct {
		ID                int64           `json:"id"`
		UID               string          `json:"uid"`
		OriginInstanceUID string          `json:"origin_instance_uid"`
		ProjectID         int64           `json:"project_id"`
		ProjectName       string          `json:"project_name"`
		IssueID           *int64          `json:"issue_id"`
		IssueUID          *string         `json:"issue_uid"`
		IssueNumber       *int64          `json:"issue_number"`
		RelatedIssueID    *int64          `json:"related_issue_id"`
		RelatedIssueUID   *string         `json:"related_issue_uid"`
		Type              string          `json:"type"`
		Actor             string          `json:"actor"`
		Payload           json.RawMessage `json:"payload"`
		CreatedAt         string          `json:"created_at"`
	}
	scrubCondition := `(peer.id IS NULL AND events.related_issue_id IS NOT NULL)`
	if !opts.IncludeDeleted {
		scrubCondition += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
	}
	relatedIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_id END`
	relatedUIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_uid END`
	query := fmt.Sprintf(`SELECT events.id, events.uid, events.origin_instance_uid, events.project_id, %s, events.issue_id, events.issue_uid,
	                 events.issue_number, `+relatedIDExpr+`, `+relatedUIDExpr+`,
	                 events.type, events.actor, events.payload, CAST(events.created_at AS TEXT)
	          FROM events%s
	          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id`, projectNameExpr, joinProjects)
	clauses, args := eventExportWhereClauses(opts)
	clauses = append([]string{`(events.issue_id IS NULL OR subject_issue.id IS NOT NULL)`}, clauses...)
	query += whereClause(clauses) + ` ORDER BY events.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export events: %w", err)
	}
	return scanRecords(rows, KindEvent, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var payload string
		err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.ProjectName, &rec.IssueID,
			&rec.IssueUID, &rec.IssueNumber, &rec.RelatedIssueID, &rec.RelatedIssueUID,
			&rec.Type, &rec.Actor, &payload, &rec.CreatedAt)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(payload)) {
			return rec, fmt.Errorf("event %d payload is invalid JSON", rec.ID)
		}
		rec.Payload = json.RawMessage(payload)
		return rec, nil
	})
}

func exportEventsV2(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, projectNameExpr, joinProjects string) error {
	type record struct {
		ID              int64           `json:"id"`
		ProjectID       int64           `json:"project_id"`
		ProjectName     string          `json:"project_name"`
		IssueID         *int64          `json:"issue_id"`
		IssueUID        *string         `json:"issue_uid"`
		IssueNumber     *int64          `json:"issue_number"`
		RelatedIssueID  *int64          `json:"related_issue_id"`
		RelatedIssueUID *string         `json:"related_issue_uid"`
		Type            string          `json:"type"`
		Actor           string          `json:"actor"`
		Payload         json.RawMessage `json:"payload"`
		CreatedAt       string          `json:"created_at"`
	}
	// Live-only export keeps issue.links_changed events whose
	// related_issue_id points at a soft-deleted peer (the iteration-21
	// preservation rule) but the peer's `issues` row is intentionally
	// omitted, so emitting the FK as-is would dangle on import. Scrub
	// related_issue_id / related_issue_uid to NULL in that case so the
	// JSONL round-trips. The payload's *_uids slices retain the orphan
	// UID for historical context. include_deleted=true exports both
	// sides, so the FK roundtrips fine — leave the columns alone there.
	scrubCondition := `(peer.id IS NULL AND events.related_issue_id IS NOT NULL)`
	if !opts.IncludeDeleted {
		scrubCondition += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
	}
	relatedIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_id END`
	relatedUIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_uid END`
	query := fmt.Sprintf(`SELECT events.id, events.project_id, %s, events.issue_id, events.issue_uid,
	                 events.issue_number, `+relatedIDExpr+`, `+relatedUIDExpr+`,
	                 events.type, events.actor, events.payload, CAST(events.created_at AS TEXT)
	          FROM events%s
	          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id`, projectNameExpr, joinProjects)
	clauses, args := eventExportWhereClauses(opts)
	clauses = append([]string{`(events.issue_id IS NULL OR subject_issue.id IS NOT NULL)`}, clauses...)
	query += whereClause(clauses) + ` ORDER BY events.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export events: %w", err)
	}
	return scanRecords(rows, KindEvent, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var payload string
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.ProjectName, &rec.IssueID,
			&rec.IssueUID, &rec.IssueNumber, &rec.RelatedIssueID, &rec.RelatedIssueUID,
			&rec.Type, &rec.Actor, &payload, &rec.CreatedAt)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(payload)) {
			return rec, fmt.Errorf("event %d payload is invalid JSON", rec.ID)
		}
		rec.Payload = json.RawMessage(payload)
		return rec, nil
	})
}

func exportEventsV1(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, projectNameExpr, joinProjects string) error {
	type record struct {
		ID             int64           `json:"id"`
		ProjectID      int64           `json:"project_id"`
		ProjectName    string          `json:"project_name"`
		IssueID        *int64          `json:"issue_id"`
		IssueNumber    *int64          `json:"issue_number"`
		RelatedIssueID *int64          `json:"related_issue_id"`
		Type           string          `json:"type"`
		Actor          string          `json:"actor"`
		Payload        json.RawMessage `json:"payload"`
		CreatedAt      string          `json:"created_at"`
	}
	// See exportEventsV2 above for why aggregated issue.links_changed
	// events on a soft-deleted peer get their related_issue_id scrubbed
	// in live-only exports. V1 has no related_issue_uid column.
	scrubCondition := `(peer.id IS NULL AND events.related_issue_id IS NOT NULL)`
	if !opts.IncludeDeleted {
		scrubCondition += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
	}
	relatedIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_id END`
	query := fmt.Sprintf(`SELECT events.id, events.project_id, %s, events.issue_id, events.issue_number,
	                 `+relatedIDExpr+`, events.type, events.actor, events.payload, CAST(events.created_at AS TEXT)
	          FROM events%s
	          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id`, projectNameExpr, joinProjects)
	clauses, args := eventExportWhereClauses(opts)
	clauses = append([]string{`(events.issue_id IS NULL OR subject_issue.id IS NOT NULL)`}, clauses...)
	query += whereClause(clauses) + ` ORDER BY events.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export events: %w", err)
	}
	return scanRecords(rows, KindEvent, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		var payload string
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.ProjectName, &rec.IssueID,
			&rec.IssueNumber, &rec.RelatedIssueID, &rec.Type, &rec.Actor,
			&payload, &rec.CreatedAt)
		if err != nil {
			return rec, err
		}
		if !json.Valid([]byte(payload)) {
			return rec, fmt.Errorf("event %d payload is invalid JSON", rec.ID)
		}
		rec.Payload = json.RawMessage(payload)
		return rec, nil
	})
}

func exportPurgeLog(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, sourceSchemaVersion int) error {
	projectNameExpr, joinProjects, err := purgeProjectNameExpr(ctx, d, sourceSchemaVersion)
	if err != nil {
		return err
	}
	if sourceSchemaVersion < 2 {
		return exportPurgeLogV1(ctx, d, enc, opts, projectNameExpr, joinProjects)
	}
	if sourceSchemaVersion < 3 {
		return exportPurgeLogV2(ctx, d, enc, opts, projectNameExpr, joinProjects)
	}
	if sourceSchemaVersion < 8 {
		return exportPurgeLogV3(ctx, d, enc, opts, projectNameExpr, joinProjects)
	}
	type record struct {
		ID                     int64   `json:"id"`
		UID                    string  `json:"uid"`
		OriginInstanceUID      string  `json:"origin_instance_uid"`
		ProjectID              int64   `json:"project_id"`
		PurgedIssueID          int64   `json:"purged_issue_id"`
		IssueUID               *string `json:"issue_uid"`
		ProjectUID             *string `json:"project_uid"`
		ProjectName            string  `json:"project_name"`
		ShortID                *string `json:"short_id,omitempty"`
		IssueTitle             string  `json:"issue_title"`
		IssueAuthor            string  `json:"issue_author"`
		CommentCount           int64   `json:"comment_count"`
		LinkCount              int64   `json:"link_count"`
		LabelCount             int64   `json:"label_count"`
		EventCount             int64   `json:"event_count"`
		EventsDeletedMinID     *int64  `json:"events_deleted_min_id"`
		EventsDeletedMaxID     *int64  `json:"events_deleted_max_id"`
		PurgeResetAfterEventID *int64  `json:"purge_reset_after_event_id"`
		Actor                  string  `json:"actor"`
		Reason                 *string `json:"reason"`
		PurgedAt               string  `json:"purged_at"`
	}
	query := fmt.Sprintf(`SELECT purge_log.id, purge_log.uid, purge_log.origin_instance_uid, purge_log.project_id, purged_issue_id, issue_uid, project_uid,
	                 %s, short_id, issue_title,
	                 issue_author, comment_count, link_count, label_count, event_count,
	                 events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
	                 actor, reason, CAST(purged_at AS TEXT)
	          FROM purge_log%s`, projectNameExpr, joinProjects)
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE purge_log.project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY purge_log.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export purge_log: %w", err)
	}
	return scanRecords(rows, KindPurgeLog, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.PurgedIssueID, &rec.IssueUID,
			&rec.ProjectUID, &rec.ProjectName, &rec.ShortID, &rec.IssueTitle, &rec.IssueAuthor, &rec.CommentCount,
			&rec.LinkCount, &rec.LabelCount, &rec.EventCount, &rec.EventsDeletedMinID,
			&rec.EventsDeletedMaxID, &rec.PurgeResetAfterEventID, &rec.Actor, &rec.Reason,
			&rec.PurgedAt)
		return rec, err
	})
}

// exportPurgeLogV3 emits the v3..v7 purge_log projection (with the now-dropped
// issue_number column). Cutover from a pre-short_id source DB lands here.
func exportPurgeLogV3(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, projectNameExpr, joinProjects string) error {
	type record struct {
		ID                     int64   `json:"id"`
		UID                    string  `json:"uid"`
		OriginInstanceUID      string  `json:"origin_instance_uid"`
		ProjectID              int64   `json:"project_id"`
		PurgedIssueID          int64   `json:"purged_issue_id"`
		IssueUID               *string `json:"issue_uid"`
		ProjectUID             *string `json:"project_uid"`
		ProjectName            string  `json:"project_name"`
		IssueNumber            int64   `json:"issue_number"`
		IssueTitle             string  `json:"issue_title"`
		IssueAuthor            string  `json:"issue_author"`
		CommentCount           int64   `json:"comment_count"`
		LinkCount              int64   `json:"link_count"`
		LabelCount             int64   `json:"label_count"`
		EventCount             int64   `json:"event_count"`
		EventsDeletedMinID     *int64  `json:"events_deleted_min_id"`
		EventsDeletedMaxID     *int64  `json:"events_deleted_max_id"`
		PurgeResetAfterEventID *int64  `json:"purge_reset_after_event_id"`
		Actor                  string  `json:"actor"`
		Reason                 *string `json:"reason"`
		PurgedAt               string  `json:"purged_at"`
	}
	query := fmt.Sprintf(`SELECT purge_log.id, purge_log.uid, purge_log.origin_instance_uid, purge_log.project_id, purged_issue_id, issue_uid, project_uid,
	                 %s, issue_number, issue_title,
	                 issue_author, comment_count, link_count, label_count, event_count,
	                 events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
	                 actor, reason, CAST(purged_at AS TEXT)
	          FROM purge_log%s`, projectNameExpr, joinProjects)
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE purge_log.project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY purge_log.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export purge_log: %w", err)
	}
	return scanRecords(rows, KindPurgeLog, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.PurgedIssueID, &rec.IssueUID,
			&rec.ProjectUID, &rec.ProjectName, &rec.IssueNumber, &rec.IssueTitle, &rec.IssueAuthor, &rec.CommentCount,
			&rec.LinkCount, &rec.LabelCount, &rec.EventCount, &rec.EventsDeletedMinID,
			&rec.EventsDeletedMaxID, &rec.PurgeResetAfterEventID, &rec.Actor, &rec.Reason,
			&rec.PurgedAt)
		return rec, err
	})
}

func exportPurgeLogV2(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, projectNameExpr, joinProjects string) error {
	type record struct {
		ID                     int64   `json:"id"`
		ProjectID              int64   `json:"project_id"`
		PurgedIssueID          int64   `json:"purged_issue_id"`
		IssueUID               *string `json:"issue_uid"`
		ProjectUID             *string `json:"project_uid"`
		ProjectName            string  `json:"project_name"`
		IssueNumber            int64   `json:"issue_number"`
		IssueTitle             string  `json:"issue_title"`
		IssueAuthor            string  `json:"issue_author"`
		CommentCount           int64   `json:"comment_count"`
		LinkCount              int64   `json:"link_count"`
		LabelCount             int64   `json:"label_count"`
		EventCount             int64   `json:"event_count"`
		EventsDeletedMinID     *int64  `json:"events_deleted_min_id"`
		EventsDeletedMaxID     *int64  `json:"events_deleted_max_id"`
		PurgeResetAfterEventID *int64  `json:"purge_reset_after_event_id"`
		Actor                  string  `json:"actor"`
		Reason                 *string `json:"reason"`
		PurgedAt               string  `json:"purged_at"`
	}
	query := fmt.Sprintf(`SELECT purge_log.id, purge_log.project_id, purged_issue_id, issue_uid, project_uid,
	                 %s, issue_number, issue_title,
	                 issue_author, comment_count, link_count, label_count, event_count,
	                 events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
	                 actor, reason, CAST(purged_at AS TEXT)
	          FROM purge_log%s`, projectNameExpr, joinProjects)
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE purge_log.project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY purge_log.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export purge_log: %w", err)
	}
	return scanRecords(rows, KindPurgeLog, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.PurgedIssueID, &rec.IssueUID,
			&rec.ProjectUID, &rec.ProjectName, &rec.IssueNumber, &rec.IssueTitle, &rec.IssueAuthor, &rec.CommentCount,
			&rec.LinkCount, &rec.LabelCount, &rec.EventCount, &rec.EventsDeletedMinID,
			&rec.EventsDeletedMaxID, &rec.PurgeResetAfterEventID, &rec.Actor, &rec.Reason,
			&rec.PurgedAt)
		return rec, err
	})
}

func exportPurgeLogV1(ctx context.Context, d *sqlitestore.Store, enc *Encoder, opts ExportOptions, projectNameExpr, joinProjects string) error {
	type record struct {
		ID                     int64   `json:"id"`
		ProjectID              int64   `json:"project_id"`
		PurgedIssueID          int64   `json:"purged_issue_id"`
		ProjectName            string  `json:"project_name"`
		IssueNumber            int64   `json:"issue_number"`
		IssueTitle             string  `json:"issue_title"`
		IssueAuthor            string  `json:"issue_author"`
		CommentCount           int64   `json:"comment_count"`
		LinkCount              int64   `json:"link_count"`
		LabelCount             int64   `json:"label_count"`
		EventCount             int64   `json:"event_count"`
		EventsDeletedMinID     *int64  `json:"events_deleted_min_id"`
		EventsDeletedMaxID     *int64  `json:"events_deleted_max_id"`
		PurgeResetAfterEventID *int64  `json:"purge_reset_after_event_id"`
		Actor                  string  `json:"actor"`
		Reason                 *string `json:"reason"`
		PurgedAt               string  `json:"purged_at"`
	}
	query := fmt.Sprintf(`SELECT purge_log.id, purge_log.project_id, purged_issue_id, %s, issue_number, issue_title,
	                 issue_author, comment_count, link_count, label_count, event_count,
	                 events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
	                 actor, reason, CAST(purged_at AS TEXT)
	          FROM purge_log%s`, projectNameExpr, joinProjects)
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE purge_log.project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY purge_log.id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export purge_log: %w", err)
	}
	return scanRecords(rows, KindPurgeLog, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.PurgedIssueID,
			&rec.ProjectName, &rec.IssueNumber, &rec.IssueTitle, &rec.IssueAuthor, &rec.CommentCount,
			&rec.LinkCount, &rec.LabelCount, &rec.EventCount, &rec.EventsDeletedMinID,
			&rec.EventsDeletedMaxID, &rec.PurgeResetAfterEventID, &rec.Actor, &rec.Reason,
			&rec.PurgedAt)
		return rec, err
	})
}

func tableHasColumn(ctx context.Context, d *sqlitestore.Store, table, column string) (bool, error) {
	rows, err := d.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	return false, nil
}

func eventProjectNameExpr(sourceSchemaVersion int) (expr, joinProjects string) {
	if sourceSchemaVersion < 7 {
		return "projects.name", " JOIN projects ON projects.id = events.project_id"
	}
	return "events.project_name", ""
}

func purgeProjectNameExpr(ctx context.Context, d *sqlitestore.Store, sourceSchemaVersion int) (expr, joinProjects string, err error) {
	if sourceSchemaVersion < 7 {
		hasLegacy, err := tableHasColumn(ctx, d, "purge_log", "project_identity")
		if err != nil {
			return "", "", err
		}
		if hasLegacy {
			return "COALESCE(projects.name, purge_log.project_identity)", " LEFT JOIN projects ON projects.id = purge_log.project_id", nil
		}
	}
	return "purge_log.project_name", "", nil
}

func issueExportWhere(table string, opts ExportOptions) (string, []any) {
	clauses := []string{}
	args := []any{}
	if opts.ProjectID > 0 {
		clauses = append(clauses, table+`.project_id = ?`)
		args = append(args, opts.ProjectID)
	}
	if !opts.IncludeDeleted {
		clauses = append(clauses, table+`.deleted_at IS NULL`)
	}
	return whereClause(clauses), args
}

func linkExportWhere(opts ExportOptions) (string, []any) {
	clauses := []string{}
	args := []any{}
	if opts.ProjectID > 0 {
		clauses = append(clauses, `links.project_id = ?`)
		args = append(args, opts.ProjectID)
	}
	if !opts.IncludeDeleted {
		clauses = append(clauses, `from_issues.deleted_at IS NULL`, `to_issues.deleted_at IS NULL`)
	}
	return whereClause(clauses), args
}

// eventExportWhereClauses returns the individual WHERE clauses (not a joined
// string like its issueExportWhere / linkExportWhere siblings) so callers can
// prepend the JOIN-dependent subject_issue orphan filter before assembling
// the final WHERE. The clauses below cover the soft-delete dimension; orphan
// filtering rides on the subject_issue / peer LEFT JOINs in each variant.
func eventExportWhereClauses(opts ExportOptions) ([]string, []any) {
	clauses := []string{}
	args := []any{}
	if opts.ProjectID > 0 {
		clauses = append(clauses, `events.project_id = ?`)
		args = append(args, opts.ProjectID)
	}
	if !opts.IncludeDeleted {
		// See exportEventsV2 / exportEvents commentary for the
		// kata#1 design call: aggregated issue.links_changed events
		// retain related_issue_id pointing at a soft-deleted peer
		// so historical context survives. Per-link issue.linked /
		// issue.unlinked events still drop via related_issue_id when
		// the peer is *soft-deleted* — but a fully-missing peer
		// (orphan FK) must pass through to the SELECT-side CASE
		// scrub so the event survives with NULL related fields,
		// matching the issue_id-orphan behavior.
		clauses = append(clauses,
			`(events.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues WHERE issues.id = events.issue_id AND issues.deleted_at IS NULL))`,
			`(events.related_issue_id IS NULL OR events.type = 'issue.links_changed' OR NOT EXISTS (SELECT 1 FROM issues WHERE issues.id = events.related_issue_id AND issues.deleted_at IS NOT NULL))`,
		)
	}
	return clauses, args
}

func whereClause(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return " WHERE " + joinClauses(clauses)
}

func joinClauses(clauses []string) string {
	out := clauses[0]
	for _, clause := range clauses[1:] {
		out += " AND " + clause
	}
	return out
}

func exportSQLiteSequence(ctx context.Context, d *sqlitestore.Store, enc *Encoder) error {
	type record struct {
		Name string `json:"name"`
		Seq  int64  `json:"seq"`
	}
	rows, err := d.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence ORDER BY name ASC`)
	if err != nil {
		return fmt.Errorf("export sqlite_sequence: %w", err)
	}
	return scanRecords(rows, KindSQLiteSequence, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.Name, &rec.Seq)
		return rec, err
	})
}

func scanRecords[T any](rows *sql.Rows, kind Kind, enc *Encoder, scan func(*sql.Rows) (T, error)) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		rec, err := scan(rows)
		if err != nil {
			return fmt.Errorf("scan %s: %w", kind, err)
		}
		if err := writeRecord(enc, kind, rec); err != nil {
			return err
		}
	}
	return rows.Err()
}
