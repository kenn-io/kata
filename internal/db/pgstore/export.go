package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"go.kenn.io/kata/internal/db"
)

func streamExportRows[T any](
	ctx context.Context,
	store *Store,
	label string,
	query string,
	args []any,
	scan func(*sql.Rows) (T, error),
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := store.exportQueryContext(ctx, query, args...)
		if err != nil {
			yield(zero, fmt.Errorf("export %s: %w", label, mapSQLError(err, nil)))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			record, err := scan(rows)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(record, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, fmt.Errorf("export %s: %w", label, mapSQLError(err, nil)))
		}
	}
}

func pgExportWhere(table string, filter db.ExportFilter) (string, []any) {
	var clauses []string
	var args []any
	if filter.ProjectID != nil {
		clauses = append(clauses, table+`.project_id = $1`)
		args = append(args, *filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses, table+`.deleted_at IS NULL`)
	}
	return pgJoinClauses(clauses), args
}

func pgProjectFilter(query, column string, filter db.ExportFilter) (string, []any) {
	if filter.ProjectID == nil {
		return query, nil
	}
	return query + ` WHERE ` + column + ` = $1`, []any{*filter.ProjectID}
}

func pgJoinClauses(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(clauses, " AND ")
}

func pgExportScanError(entity string, err error) error {
	return fmt.Errorf("scan %s: %w", entity, mapSQLError(err, nil))
}

func pgInvalidJSONError(entity string, id int64, field string) error {
	return fmt.Errorf("%s %d %s is invalid JSON", entity, id, field)
}

// ExportSequences translates Postgres identity high-water marks into the
// backend-neutral sequence records consumed by SQLite restores. Sequence
// advancement is intentionally allowed to over-reserve IDs after restore;
// reuse below a purge tombstone or reset cursor is not.
func (s *Store) ExportSequences(ctx context.Context) iter.Seq2[db.SequenceExport, error] {
	const query = `WITH identity_tables(name) AS (VALUES
('projects'),('project_aliases'),('recurrences'),('issue_sync_bindings'),('issues'),
('comments'),('links'),('import_mappings'),('events'),('purge_log'),
('project_purge_log'),('api_tokens'),('federation_quarantine'),
('federation_enrollments'),('issue_claims'),('pending_claim_requests')
)
SELECT t.name, COALESCE(s.last_value,0)
FROM identity_tables t
JOIN pg_sequences s
  ON s.schemaname=current_schema()
 AND to_regclass(format('%I.%I',s.schemaname,s.sequencename))=
     to_regclass(pg_get_serial_sequence(format('%I.%I',current_schema(),t.name),'id'))
WHERE COALESCE(s.last_value,0)>0
ORDER BY t.name ASC`
	return streamExportRows(ctx, s, "identity sequences", query, nil,
		func(rows *sql.Rows) (db.SequenceExport, error) {
			var record db.SequenceExport
			if err := rows.Scan(&record.Name, &record.Seq); err != nil {
				return db.SequenceExport{}, pgExportScanError("identity sequence", err)
			}
			return record, nil
		})
}

// ExportPurgeLog streams issue-purge tombstones in durable identity order.
func (s *Store) ExportPurgeLog(
	ctx context.Context,
	filter db.ExportFilter,
) iter.Seq2[db.PurgeLogExport, error] {
	query, args := pgProjectFilter(`SELECT id,uid,origin_instance_uid,project_id,purged_issue_id,
issue_uid,project_uid,project_name,short_id,issue_title,issue_author,comment_count,link_count,
label_count,event_count,events_deleted_min_id,events_deleted_max_id,purge_reset_after_event_id,
actor,reason,purged_at FROM purge_log`, "project_id", filter)
	query += ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "purge_log", query, args,
		func(rows *sql.Rows) (db.PurgeLogExport, error) {
			var record db.PurgeLogExport
			if err := rows.Scan(&record.ID, &record.UID, &record.OriginInstanceUID,
				&record.ProjectID, &record.PurgedIssueID, &record.IssueUID, &record.ProjectUID,
				&record.ProjectName, &record.ShortID, &record.IssueTitle, &record.IssueAuthor,
				&record.CommentCount, &record.LinkCount, &record.LabelCount, &record.EventCount,
				&record.EventsDeletedMinID, &record.EventsDeletedMaxID,
				&record.PurgeResetAfterEventID, &record.Actor, &record.Reason,
				&record.PurgedAt); err != nil {
				return db.PurgeLogExport{}, pgExportScanError("purge_log", err)
			}
			return record, nil
		})
}

// ExportProjectPurgeLog streams project-purge tombstones in durable identity order.
func (s *Store) ExportProjectPurgeLog(
	ctx context.Context,
	filter db.ExportFilter,
) iter.Seq2[db.ProjectPurgeLogExport, error] {
	query, args := pgProjectFilter(`SELECT id,uid,origin_instance_uid,project_id,project_uid,
project_name,issue_count,event_count,alias_count,comment_count,link_count,label_count,
claim_count,pending_claim_request_count,events_deleted_min_id,events_deleted_max_id,
purge_reset_after_event_id,actor,reason,
to_char(purged_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
FROM project_purge_log`, "project_id", filter)
	query += ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "project_purge_log", query, args,
		func(rows *sql.Rows) (db.ProjectPurgeLogExport, error) {
			var record db.ProjectPurgeLogExport
			if err := rows.Scan(&record.ID, &record.UID, &record.OriginInstanceUID,
				&record.ProjectID, &record.ProjectUID, &record.ProjectName, &record.IssueCount,
				&record.EventCount, &record.AliasCount, &record.CommentCount, &record.LinkCount,
				&record.LabelCount, &record.ClaimCount, &record.PendingClaimRequestCount,
				&record.EventsDeletedMinID, &record.EventsDeletedMaxID,
				&record.PurgeResetAfterEventID, &record.Actor, &record.Reason,
				&record.PurgedAt); err != nil {
				return db.ProjectPurgeLogExport{}, pgExportScanError("project_purge_log", err)
			}
			return record, nil
		})
}

// ExportMeta streams installation metadata ordered by key.
func (s *Store) ExportMeta(ctx context.Context) iter.Seq2[db.MetaKV, error] {
	return streamExportRows(ctx, s, "meta", `SELECT key, value FROM meta ORDER BY key ASC`, nil,
		func(rows *sql.Rows) (db.MetaKV, error) {
			var record db.MetaKV
			if err := rows.Scan(&record.Key, &record.Value); err != nil {
				return db.MetaKV{}, pgExportScanError("meta", err)
			}
			return record, nil
		})
}

// ExportProjects streams projects ordered by identity, optionally scoped to one project.
func (s *Store) ExportProjects(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.ProjectExport, error] {
	query, args := pgProjectFilter(
		`SELECT id, uid, name, created_at, deleted_at, metadata, revision FROM projects`,
		"id", filter,
	)
	query += ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "projects", query, args,
		func(rows *sql.Rows) (db.ProjectExport, error) {
			var record db.ProjectExport
			var metadata string
			if err := rows.Scan(&record.ID, &record.UID, &record.Name, &record.CreatedAt,
				&record.DeletedAt, &metadata, &record.Revision); err != nil {
				return db.ProjectExport{}, pgExportScanError("project", err)
			}
			if !json.Valid([]byte(metadata)) {
				return db.ProjectExport{}, pgInvalidJSONError("project", record.ID, "metadata")
			}
			record.Metadata = json.RawMessage(metadata)
			return record, nil
		})
}

// ExportIssues streams project-scoped issue projections in identity order.
func (s *Store) ExportIssues(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.IssueExport, error] {
	where, args := pgExportWhere("i", filter)
	query := `SELECT i.id, i.uid, i.project_id, i.short_id, i.title, i.body,
       i.status, i.closed_reason, i.owner, i.priority, i.author,
       i.created_at, i.updated_at, i.closed_at, i.deleted_at,
       i.metadata, i.revision, i.content_revision, i.recurrence_id, r.uid, i.occurrence_key
  FROM issues i LEFT JOIN recurrences r ON r.id = i.recurrence_id` + where + ` ORDER BY i.id ASC`
	return streamExportRows(ctx, s, "issues", query, args,
		func(rows *sql.Rows) (db.IssueExport, error) {
			var record db.IssueExport
			var metadata string
			if err := rows.Scan(&record.ID, &record.UID, &record.ProjectID, &record.ShortID,
				&record.Title, &record.Body, &record.Status, &record.ClosedReason, &record.Owner,
				&record.Priority, &record.Author, &record.CreatedAt, &record.UpdatedAt,
				&record.ClosedAt, &record.DeletedAt, &metadata, &record.Revision,
				&record.ContentRevision, &record.RecurrenceID, &record.RecurrenceUID,
				&record.OccurrenceKey); err != nil {
				return db.IssueExport{}, pgExportScanError("issue", err)
			}
			if !json.Valid([]byte(metadata)) {
				return db.IssueExport{}, pgInvalidJSONError("issue", record.ID, "metadata")
			}
			record.Metadata = json.RawMessage(metadata)
			return record, nil
		})
}

// ExportRecurrences streams recurrence definitions needed by exported issues.
func (s *Store) ExportRecurrences(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.RecurrenceExport, error] {
	query := `SELECT id, uid, project_id, rrule, dtstart, timezone,
       template_title, template_body, template_owner, template_priority,
       template_labels, template_metadata, next_occurrence_key, last_materialized_uid,
       author, revision, created_at, updated_at, deleted_at FROM recurrences`
	var clauses []string
	var args []any
	if filter.ProjectID != nil {
		clauses = append(clauses, `project_id = $1`)
		args = append(args, *filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses, `(deleted_at IS NULL OR id IN (
SELECT DISTINCT recurrence_id FROM issues WHERE recurrence_id IS NOT NULL AND deleted_at IS NULL))`)
	}
	query += pgJoinClauses(clauses) + ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "recurrences", query, args,
		func(rows *sql.Rows) (db.RecurrenceExport, error) {
			var record db.RecurrenceExport
			var labels, metadata string
			if err := rows.Scan(&record.ID, &record.UID, &record.ProjectID, &record.RRule,
				&record.DTStart, &record.Timezone, &record.TemplateTitle, &record.TemplateBody,
				&record.TemplateOwner, &record.TemplatePriority, &labels, &metadata,
				&record.NextOccurrenceKey, &record.LastMaterializedUID, &record.Author,
				&record.Revision, &record.CreatedAt, &record.UpdatedAt, &record.DeletedAt); err != nil {
				return db.RecurrenceExport{}, pgExportScanError("recurrence", err)
			}
			if !json.Valid([]byte(labels)) {
				return db.RecurrenceExport{}, pgInvalidJSONError("recurrence", record.ID, "template_labels")
			}
			if !json.Valid([]byte(metadata)) {
				return db.RecurrenceExport{}, pgInvalidJSONError("recurrence", record.ID, "template_metadata")
			}
			record.TemplateLabels = json.RawMessage(labels)
			record.TemplateMetadata = json.RawMessage(metadata)
			return record, nil
		})
}

// ExportComments streams comments whose parent issues pass the export filter.
func (s *Store) ExportComments(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.CommentExport, error] {
	where, args := pgExportWhere("issues", filter)
	query := `SELECT comments.id, comments.uid, comments.issue_id, comments.author,
       comments.body, comments.created_at
  FROM comments JOIN issues ON issues.id = comments.issue_id` + where + ` ORDER BY comments.id ASC`
	return streamExportRows(ctx, s, "comments", query, args,
		func(rows *sql.Rows) (db.CommentExport, error) {
			var record db.CommentExport
			if err := rows.Scan(&record.ID, &record.UID, &record.IssueID, &record.Author,
				&record.Body, &record.CreatedAt); err != nil {
				return db.CommentExport{}, pgExportScanError("comment", err)
			}
			return record, nil
		})
}

// ExportIssueLabels streams labels whose parent issues pass the export filter.
func (s *Store) ExportIssueLabels(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.IssueLabelExport, error] {
	where, args := pgExportWhere("issues", filter)
	query := `SELECT issue_labels.issue_id, issue_labels.label, issue_labels.author,
       issue_labels.created_at
  FROM issue_labels JOIN issues ON issues.id = issue_labels.issue_id` + where +
		` ORDER BY issue_labels.issue_id ASC, issue_labels.label ASC`
	return streamExportRows(ctx, s, "issue_labels", query, args,
		func(rows *sql.Rows) (db.IssueLabelExport, error) {
			var record db.IssueLabelExport
			if err := rows.Scan(&record.IssueID, &record.Label, &record.Author, &record.CreatedAt); err != nil {
				return db.IssueLabelExport{}, pgExportScanError("issue_label", err)
			}
			return record, nil
		})
}

// ExportLinks streams relationships whose endpoints pass the export filter.
func (s *Store) ExportLinks(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.LinkExport, error] {
	query := `SELECT links.id, links.from_issue_id, links.from_issue_uid,
       links.to_issue_id, links.to_issue_uid, links.type, links.author, links.created_at
  FROM links
  JOIN issues from_issues ON from_issues.id = links.from_issue_id
  JOIN issues to_issues ON to_issues.id = links.to_issue_id`
	var clauses []string
	var args []any
	if filter.ProjectID != nil {
		clauses = append(clauses, `(from_issues.project_id = $1 OR to_issues.project_id = $1)`)
		args = append(args, *filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses, `from_issues.deleted_at IS NULL`, `to_issues.deleted_at IS NULL`)
	}
	query += pgJoinClauses(clauses) + ` ORDER BY links.id ASC`
	return streamExportRows(ctx, s, "links", query, args,
		func(rows *sql.Rows) (db.LinkExport, error) {
			var record db.LinkExport
			if err := rows.Scan(&record.ID, &record.FromIssueID, &record.FromIssueUID,
				&record.ToIssueID, &record.ToIssueUID, &record.Type, &record.Author,
				&record.CreatedAt); err != nil {
				return db.LinkExport{}, pgExportScanError("link", err)
			}
			return record, nil
		})
}

// ExportProjectAliases streams project aliases in identity order.
func (s *Store) ExportProjectAliases(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.AliasExport, error] {
	query, args := pgProjectFilter(
		`SELECT id, project_id, alias_identity, alias_kind, created_at FROM project_aliases`,
		"project_id", filter,
	)
	query += ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "project_aliases", query, args,
		func(rows *sql.Rows) (db.AliasExport, error) {
			var record db.AliasExport
			if err := rows.Scan(&record.ID, &record.ProjectID, &record.AliasIdentity,
				&record.AliasKind, &record.CreatedAt); err != nil {
				return db.AliasExport{}, pgExportScanError("project_alias", err)
			}
			return record, nil
		})
}

// ExportImportMappings streams source identities retained by the selected project data.
func (s *Store) ExportImportMappings(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.ImportMappingExport, error] {
	query := `SELECT id, source, external_id, object_type, project_id, issue_id,
       comment_id, link_id, label, source_updated_at, imported_at FROM import_mappings`
	var clauses []string
	var args []any
	if filter.ProjectID != nil {
		clauses = append(clauses, `project_id = $1`)
		args = append(args, *filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses,
			`(object_type NOT IN ('issue','comment','label') OR EXISTS (
SELECT 1 FROM issues WHERE issues.id = import_mappings.issue_id AND issues.deleted_at IS NULL))`,
			`(object_type <> 'link' OR EXISTS (
SELECT 1 FROM links
JOIN issues from_issues ON from_issues.id = links.from_issue_id
JOIN issues to_issues ON to_issues.id = links.to_issue_id
WHERE links.id = import_mappings.link_id
  AND from_issues.deleted_at IS NULL AND to_issues.deleted_at IS NULL))`,
		)
	}
	query += pgJoinClauses(clauses) + ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "import_mappings", query, args,
		func(rows *sql.Rows) (db.ImportMappingExport, error) {
			var record db.ImportMappingExport
			if err := rows.Scan(&record.ID, &record.Source, &record.ExternalID, &record.ObjectType,
				&record.ProjectID, &record.IssueID, &record.CommentID, &record.LinkID, &record.Label,
				&record.SourceUpdatedAt, &record.ImportedAt); err != nil {
				return db.ImportMappingExport{}, pgExportScanError("import_mapping", err)
			}
			return record, nil
		})
}

// ExportIssueSyncBindings streams provider bindings for selected projects.
func (s *Store) ExportIssueSyncBindings(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.IssueSyncBindingExport, error] {
	query, args := pgProjectFilter(`SELECT id, project_id, provider, source_key, remote_id,
       display_name, config_json, enabled, interval_seconds, last_cursor_at, created_at, updated_at
  FROM issue_sync_bindings`, "project_id", filter)
	query += ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "issue_sync_bindings", query, args,
		func(rows *sql.Rows) (db.IssueSyncBindingExport, error) {
			var record db.IssueSyncBindingExport
			var config string
			var enabled int
			if err := rows.Scan(&record.ID, &record.ProjectID, &record.Provider, &record.SourceKey,
				&record.RemoteID, &record.DisplayName, &config, &enabled,
				&record.IntervalSeconds, &record.LastCursorAt, &record.CreatedAt,
				&record.UpdatedAt); err != nil {
				return db.IssueSyncBindingExport{}, pgExportScanError("issue_sync_binding", err)
			}
			if !json.Valid([]byte(config)) {
				return db.IssueSyncBindingExport{}, pgInvalidJSONError("issue_sync_binding", record.ID, "config_json")
			}
			record.Config = json.RawMessage(config)
			record.Enabled = enabled == 1
			return record, nil
		})
}

// ExportIssueSyncStatus streams provider worker state for selected projects.
func (s *Store) ExportIssueSyncStatus(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.IssueSyncStatusExport, error] {
	query, args := pgProjectFilter(`SELECT binding_id, project_id, sync_started_at,
       last_attempt_at, last_success_at, last_error_at, last_error, last_created,
       last_updated, last_unchanged, last_comments FROM issue_sync_status`, "project_id", filter)
	query += ` ORDER BY binding_id ASC`
	return streamExportRows(ctx, s, "issue_sync_status", query, args,
		func(rows *sql.Rows) (db.IssueSyncStatusExport, error) {
			var record db.IssueSyncStatusExport
			if err := rows.Scan(&record.BindingID, &record.ProjectID, &record.SyncStartedAt,
				&record.LastAttemptAt, &record.LastSuccessAt, &record.LastErrorAt,
				&record.LastError, &record.LastCreated, &record.LastUpdated,
				&record.LastUnchanged, &record.LastComments); err != nil {
				return db.IssueSyncStatusExport{}, pgExportScanError("issue_sync_status", err)
			}
			return record, nil
		})
}

// ExportFederationBindings streams federation bindings in project order.
func (s *Store) ExportFederationBindings(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.FederationBindingExport, error] {
	query, args := pgProjectFilter(`SELECT project_id, role, hub_url, hub_project_id,
       hub_project_uid, replay_horizon_event_id, pull_cursor_event_id, push_enabled,
       push_cursor_event_id, bound_actor, allow_insecure, enabled, created_at,
       updated_at, last_sync_at FROM federation_bindings`, "project_id", filter)
	query += ` ORDER BY project_id ASC`
	return streamExportRows(ctx, s, "federation_bindings", query, args,
		func(rows *sql.Rows) (db.FederationBindingExport, error) {
			var record db.FederationBindingExport
			var pushEnabled, allowInsecure, enabled int
			if err := rows.Scan(&record.ProjectID, &record.Role, &record.HubURL,
				&record.HubProjectID, &record.HubProjectUID, &record.ReplayHorizonEventID,
				&record.PullCursorEventID, &pushEnabled, &record.PushCursorEventID,
				&record.Actor, &allowInsecure, &enabled, &record.CreatedAt,
				&record.UpdatedAt, &record.LastSyncAt); err != nil {
				return db.FederationBindingExport{}, pgExportScanError("federation_binding", err)
			}
			record.PushEnabled = pushEnabled == 1
			record.AllowInsecure = allowInsecure == 1
			record.Enabled = enabled == 1
			return record, nil
		})
}

// ExportFederationSyncStatus streams worker status in project order.
func (s *Store) ExportFederationSyncStatus(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.FederationSyncStatusExport, error] {
	query, args := pgProjectFilter(`SELECT project_id, last_pull_started_at,
       last_pull_success_at, last_push_started_at, last_push_success_at,
       last_error_at, last_error, last_reset_at FROM federation_sync_status`, "project_id", filter)
	query += ` ORDER BY project_id ASC`
	return streamExportRows(ctx, s, "federation_sync_status", query, args,
		func(rows *sql.Rows) (db.FederationSyncStatusExport, error) {
			var record db.FederationSyncStatusExport
			if err := rows.Scan(&record.ProjectID, &record.LastPullStartedAt,
				&record.LastPullSuccessAt, &record.LastPushStartedAt, &record.LastPushSuccessAt,
				&record.LastErrorAt, &record.LastError, &record.LastResetAt); err != nil {
				return db.FederationSyncStatusExport{}, pgExportScanError("federation_sync_status", err)
			}
			return record, nil
		})
}

// ExportFederationQuarantine streams quarantine history in identity order.
func (s *Store) ExportFederationQuarantine(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.FederationQuarantineExport, error] {
	query, args := pgProjectFilter(`SELECT id, project_id, direction, first_event_id,
       last_event_id, event_uids, error, created_at, skipped_at, skipped_by,
       skip_reason FROM federation_quarantine`, "project_id", filter)
	query += ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "federation_quarantine", query, args,
		func(rows *sql.Rows) (db.FederationQuarantineExport, error) {
			var record db.FederationQuarantineExport
			var eventUIDs string
			if err := rows.Scan(&record.ID, &record.ProjectID, &record.Direction,
				&record.FirstEventID, &record.LastEventID, &eventUIDs, &record.Error,
				&record.CreatedAt, &record.SkippedAt, &record.SkippedBy,
				&record.SkipReason); err != nil {
				return db.FederationQuarantineExport{}, pgExportScanError("federation_quarantine", err)
			}
			if !json.Valid([]byte(eventUIDs)) {
				return db.FederationQuarantineExport{}, pgInvalidJSONError("federation_quarantine", record.ID, "event_uids")
			}
			record.EventUIDs = json.RawMessage(eventUIDs)
			return record, nil
		})
}

// ExportFederationEnrollments streams enrollment history in identity order.
func (s *Store) ExportFederationEnrollments(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.FederationEnrollmentExport, error] {
	query := `SELECT id, token_hash, spoke_instance_uid, project_id, capabilities,
       bound_actor, allow_adoption_snapshot_authors, adoption_baseline_open,
       adoption_baseline_next_source_event_id, adoption_baseline_end_source_event_id,
       created_at, updated_at, revoked_at FROM federation_enrollments`
	var args []any
	if filter.ProjectID != nil {
		query += ` WHERE project_id=$1`
		args = append(args, *filter.ProjectID)
	}
	query += ` ORDER BY id ASC`
	return streamExportRows(ctx, s, "federation_enrollments", query, args,
		func(rows *sql.Rows) (db.FederationEnrollmentExport, error) {
			var record db.FederationEnrollmentExport
			var allowAuthors, baselineOpen int
			if err := rows.Scan(&record.ID, &record.TokenHash, &record.SpokeInstanceUID,
				&record.ProjectID, &record.Capabilities, &record.Actor, &allowAuthors,
				&baselineOpen, &record.AdoptionBaselineNextSourceEventID,
				&record.AdoptionBaselineEndSourceEventID, &record.CreatedAt,
				&record.UpdatedAt, &record.RevokedAt); err != nil {
				return db.FederationEnrollmentExport{}, pgExportScanError("federation_enrollment", err)
			}
			record.AllowAdoptionSnapshotAuthors = allowAuthors == 1
			record.AdoptionBaselineOpen = baselineOpen == 1
			return record, nil
		})
}

// ExportIssueClaims streams claim history retained by selected issues.
func (s *Store) ExportIssueClaims(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.IssueClaimExport, error] {
	query := `SELECT issue_claims.id, issue_claims.claim_uid, issue_claims.project_id,
       issue_claims.issue_id, issue_claims.issue_uid, issue_claims.holder,
       issue_claims.holder_instance_uid, issue_claims.client_kind, issue_claims.purpose,
       issue_claims.claim_kind, issue_claims.acquired_at, issue_claims.expires_at,
       issue_claims.released_at, issue_claims.release_reason, issue_claims.revision,
       issue_claims.updated_at
  FROM issue_claims JOIN issues ON issues.id=issue_claims.issue_id`
	var clauses []string
	var args []any
	if filter.ProjectID != nil {
		clauses = append(clauses, `issues.project_id=$1`)
		args = append(args, *filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses, `issues.deleted_at IS NULL`)
	}
	query += pgJoinClauses(clauses) + ` ORDER BY issue_claims.id ASC`
	return streamExportRows(ctx, s, "issue_claims", query, args,
		func(rows *sql.Rows) (db.IssueClaimExport, error) {
			var record db.IssueClaimExport
			if err := rows.Scan(&record.ID, &record.ClaimUID, &record.ProjectID, &record.IssueID,
				&record.IssueUID, &record.Holder, &record.HolderInstanceUID, &record.ClientKind,
				&record.Purpose, &record.ClaimKind, &record.AcquiredAt, &record.ExpiresAt,
				&record.ReleasedAt, &record.ReleaseReason, &record.Revision, &record.UpdatedAt); err != nil {
				return db.IssueClaimExport{}, pgExportScanError("issue_claim", err)
			}
			return record, nil
		})
}

// ExportPendingClaimRequests streams pending request history retained by selected issues.
func (s *Store) ExportPendingClaimRequests(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.PendingClaimRequestExport, error] {
	query := `SELECT pending_claim_requests.id, pending_claim_requests.request_uid,
       pending_claim_requests.project_id, pending_claim_requests.issue_id,
       pending_claim_requests.issue_uid, pending_claim_requests.holder,
       pending_claim_requests.holder_instance_uid, pending_claim_requests.client_kind,
       pending_claim_requests.claim_kind, pending_claim_requests.ttl_seconds,
       pending_claim_requests.purpose, pending_claim_requests.requested_at,
       pending_claim_requests.last_attempt_at, pending_claim_requests.last_error,
       pending_claim_requests.rejected_at, pending_claim_requests.resolved_at
  FROM pending_claim_requests JOIN issues ON issues.id=pending_claim_requests.issue_id`
	var clauses []string
	var args []any
	if filter.ProjectID != nil {
		clauses = append(clauses, `issues.project_id=$1`)
		args = append(args, *filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses, `issues.deleted_at IS NULL`)
	}
	query += pgJoinClauses(clauses) + ` ORDER BY pending_claim_requests.id ASC`
	return streamExportRows(ctx, s, "pending_claim_requests", query, args,
		func(rows *sql.Rows) (db.PendingClaimRequestExport, error) {
			var record db.PendingClaimRequestExport
			if err := rows.Scan(&record.ID, &record.RequestUID, &record.ProjectID, &record.IssueID,
				&record.IssueUID, &record.Holder, &record.HolderInstanceUID, &record.ClientKind,
				&record.ClaimKind, &record.TTLSeconds, &record.Purpose, &record.RequestedAt,
				&record.LastAttemptAt, &record.LastError, &record.RejectedAt, &record.ResolvedAt); err != nil {
				return db.PendingClaimRequestExport{}, pgExportScanError("pending_claim_request", err)
			}
			return record, nil
		})
}

// ExportEvents streams replay-safe event envelopes under the export visibility rules.
func (s *Store) ExportEvents(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.EventExport, error] {
	issueIDExpression := `events.issue_id`
	relatedScrub := `(peer.id IS NULL AND (events.related_issue_id IS NOT NULL OR events.related_issue_uid IS NOT NULL))`
	var args []any
	if filter.ProjectID != nil {
		issueIDExpression = `CASE WHEN subject_issue.id IS NOT NULL AND subject_issue.project_id <> $1 THEN NULL ELSE events.issue_id END`
		relatedScrub += ` OR (peer.id IS NOT NULL AND peer.project_id <> $1)`
		args = append(args, *filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		relatedScrub += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
	}
	relatedIDExpression := `CASE WHEN ` + relatedScrub + ` THEN NULL ELSE events.related_issue_id END`
	relatedUIDExpression := `CASE WHEN ` + relatedScrub + ` THEN NULL ELSE events.related_issue_uid END`
	subjectLive := `(events.issue_id IS NULL OR subject_issue.id IS NOT NULL)`
	if !filter.IncludeDeleted {
		subjectLive = `((events.issue_id IS NULL AND subject_issue.id IS NULL)
OR (subject_issue.id IS NOT NULL AND subject_issue.deleted_at IS NULL))`
	}
	query := `SELECT events.id, events.uid, events.origin_instance_uid, events.project_id,
       export_project.uid, events.project_name, ` + issueIDExpression + `, events.issue_uid,
       ` + relatedIDExpression + `, ` + relatedUIDExpression + `,
       events.type, events.actor, events.payload, events.hlc_physical_ms,
       events.hlc_counter, events.content_hash, events.created_at
  FROM events
  JOIN projects export_project ON export_project.id = events.project_id
  LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
    OR (events.issue_id IS NULL AND events.issue_uid IS NOT NULL AND subject_issue.uid = events.issue_uid)
  LEFT JOIN issues peer ON peer.id = events.related_issue_id
    OR (events.related_issue_id IS NULL AND events.related_issue_uid IS NOT NULL AND peer.uid = events.related_issue_uid)`
	clauses := []string{subjectLive}
	if filter.ProjectID != nil {
		clauses = append(clauses, `events.project_id = $1`)
	}
	if !filter.IncludeDeleted {
		clauses = append(clauses,
			`(events.issue_id IS NULL OR EXISTS (
SELECT 1 FROM issues WHERE issues.id = events.issue_id AND issues.deleted_at IS NULL))`,
			`(events.type = 'issue.links_changed' OR (
(events.related_issue_id IS NULL OR NOT EXISTS (
  SELECT 1 FROM issues WHERE issues.id = events.related_issue_id AND issues.deleted_at IS NOT NULL))
AND (events.related_issue_uid IS NULL OR NOT EXISTS (
  SELECT 1 FROM issues WHERE issues.uid = events.related_issue_uid AND issues.deleted_at IS NOT NULL))))`,
		)
	}
	query += pgJoinClauses(clauses) + ` ORDER BY events.id ASC`
	return streamExportRows(ctx, s, "events", query, args,
		func(rows *sql.Rows) (db.EventExport, error) {
			var record db.EventExport
			var payload string
			if err := rows.Scan(&record.ID, &record.UID, &record.OriginInstanceUID,
				&record.ProjectID, &record.ProjectUID, &record.ProjectName, &record.IssueID,
				&record.IssueUID, &record.RelatedIssueID, &record.RelatedIssueUID,
				&record.Type, &record.Actor, &payload, &record.HLCPhysicalMS,
				&record.HLCCounter, &record.ContentHash, &record.CreatedAt); err != nil {
				return db.EventExport{}, pgExportScanError("event", err)
			}
			if !json.Valid([]byte(payload)) {
				return db.EventExport{}, pgInvalidJSONError("event", record.ID, "payload")
			}
			record.Payload = json.RawMessage(payload)
			contentHash, err := db.EventContentHash(db.EventHashInput{
				UID: record.UID, OriginInstanceUID: record.OriginInstanceUID,
				ProjectUID: record.ProjectUID, ProjectName: record.ProjectName,
				IssueUID: record.IssueUID, RelatedIssueUID: record.RelatedIssueUID,
				Type: record.Type, Actor: record.Actor, HLCPhysicalMS: record.HLCPhysicalMS,
				HLCCounter: record.HLCCounter, CreatedAt: record.CreatedAt, Payload: record.Payload,
			})
			if err != nil {
				return db.EventExport{}, fmt.Errorf("event %d content hash: %w", record.ID, err)
			}
			record.ContentHash = contentHash
			return record, nil
		})
}
