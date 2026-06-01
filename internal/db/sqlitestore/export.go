package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// streamRows runs query against readQ and yields each row scanned via scan.
// label is the entity name used in query-error and rows.Err wraps ("export
// <label>"); the scan callback is responsible for wrapping its own errors.
// Closing the row set and propagating rows.Err live here so every Export*
// method shares one loop discipline instead of 18 hand-rolled copies.
func streamRows[T any](
	ctx context.Context,
	readQ readQuerier,
	label string,
	query string,
	args []any,
	scan func(*sql.Rows) (T, error),
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := readQ.QueryContext(ctx, query, args...)
		if err != nil {
			yield(zero, fmt.Errorf("export %s: %w", label, err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			rec, err := scan(rows)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(rec, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, fmt.Errorf("export %s: %w", label, err))
		}
	}
}

// exportScopeClauses builds the standard "project scope + soft-delete" clauses
// for a table alias, mirroring the old issueExportWhere.
func exportScopeClauses(table string, f db.ExportFilter) []string {
	var clauses []string
	if f.ProjectID != nil {
		clauses = append(clauses, table+`.project_id = ?`)
	}
	if !f.IncludeDeleted {
		clauses = append(clauses, table+`.deleted_at IS NULL`)
	}
	return clauses
}

// exportWhere assembles a WHERE clause from exportScopeClauses.
func exportWhere(table string, f db.ExportFilter) string {
	clauses := exportScopeClauses(table, f)
	if len(clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(clauses, " AND ")
}

// exportArgs returns the args for the WHERE clause built by exportWhere.
func exportArgs(f db.ExportFilter) []any {
	if f.ProjectID != nil {
		return []any{*f.ProjectID}
	}
	return nil
}

// withProjectIDFilter appends `WHERE <col> = ?` to query when f scopes by
// project_id, returning the new query and the matching args slice. Used by
// iterators that only support the project filter (no soft-delete logic).
func withProjectIDFilter(query string, f db.ExportFilter, col string) (string, []any) {
	if f.ProjectID == nil {
		return query, nil
	}
	return query + ` WHERE ` + col + ` = ?`, []any{*f.ProjectID}
}

// joinClauses concatenates clauses with " AND " and prefixes " WHERE ".
// Returns "" for an empty slice.
func joinClauses(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(clauses, " AND ")
}

// scanError wraps a Scan failure with the canonical "scan <entity>:" prefix.
func scanError(entity string, err error) error {
	return fmt.Errorf("scan %s: %w", entity, err)
}

// invalidJSONErr is the canonical error returned when a stored JSON payload
// fails json.Valid at export time. Preserved from the original exporter; the
// schema CHECK constraints normally prevent this.
func invalidJSONErr(entity string, id int64, field string) error {
	return fmt.Errorf("%s %d %s is invalid JSON", entity, id, field)
}

// ExportMeta streams every meta row ordered by key.
func (d *Store) ExportMeta(ctx context.Context) iter.Seq2[db.MetaKV, error] {
	return streamRows(ctx, d.readQ, "meta",
		`SELECT key, value FROM meta ORDER BY key ASC`, nil,
		func(rows *sql.Rows) (db.MetaKV, error) {
			var rec db.MetaKV
			if err := rows.Scan(&rec.Key, &rec.Value); err != nil {
				return db.MetaKV{}, scanError("meta", err)
			}
			return rec, nil
		})
}

// ExportIssues streams issues ordered by id, scoped and filtered by f.
func (d *Store) ExportIssues(ctx context.Context, f db.ExportFilter) iter.Seq2[db.IssueExport, error] {
	query := `SELECT i.id, i.uid, i.project_id, i.short_id, i.title, i.body,
	                 i.status, i.closed_reason, i.owner, i.priority, i.author,
	                 CAST(i.created_at AS TEXT), CAST(i.updated_at AS TEXT),
	                 CAST(i.closed_at AS TEXT), CAST(i.deleted_at AS TEXT),
	                 i.metadata, i.revision,
	                 i.recurrence_id, r.uid, i.occurrence_key
	          FROM issues i
	          LEFT JOIN recurrences r ON r.id = i.recurrence_id` +
		exportWhere("i", f) + ` ORDER BY i.id ASC`
	return streamRows(ctx, d.readQ, "issues", query, exportArgs(f),
		func(rows *sql.Rows) (db.IssueExport, error) {
			var rec db.IssueExport
			var metadata string
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.ShortID, &rec.Title, &rec.Body,
				&rec.Status, &rec.ClosedReason, &rec.Owner, &rec.Priority, &rec.Author, &rec.CreatedAt,
				&rec.UpdatedAt, &rec.ClosedAt, &rec.DeletedAt, &metadata, &rec.Revision,
				&rec.RecurrenceID, &rec.RecurrenceUID, &rec.OccurrenceKey); err != nil {
				return db.IssueExport{}, scanError("issue", err)
			}
			if !json.Valid([]byte(metadata)) {
				return db.IssueExport{}, invalidJSONErr("issue", rec.ID, "metadata")
			}
			rec.Metadata = json.RawMessage(metadata)
			return rec, nil
		})
}

// ExportRecurrences streams recurrences ordered by id, scoped to f.ProjectID
// when set. Deleted recurrences that are still referenced by a live issue stay
// in live-only exports.
func (d *Store) ExportRecurrences(ctx context.Context, f db.ExportFilter) iter.Seq2[db.RecurrenceExport, error] {
	query := `SELECT id, uid, project_id, rrule, dtstart, timezone,
	                 template_title, template_body, template_owner, template_priority,
	                 template_labels, template_metadata,
	                 next_occurrence_key, last_materialized_uid,
	                 author, revision,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(deleted_at AS TEXT)
	          FROM recurrences`
	var clauses []string
	var args []any
	if f.ProjectID != nil {
		clauses = append(clauses, `project_id = ?`)
		args = append(args, *f.ProjectID)
	}
	if !f.IncludeDeleted {
		clauses = append(clauses, `(deleted_at IS NULL
		                        OR id IN (SELECT DISTINCT recurrence_id FROM issues
		                                   WHERE recurrence_id IS NOT NULL
		                                     AND deleted_at IS NULL))`)
	}
	query += joinClauses(clauses) + ` ORDER BY id ASC`
	return streamRows(ctx, d.readQ, "recurrences", query, args,
		func(rows *sql.Rows) (db.RecurrenceExport, error) {
			var rec db.RecurrenceExport
			var labels, metadata string
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.RRule, &rec.DTStart,
				&rec.Timezone, &rec.TemplateTitle, &rec.TemplateBody,
				&rec.TemplateOwner, &rec.TemplatePriority,
				&labels, &metadata,
				&rec.NextOccurrenceKey, &rec.LastMaterializedUID,
				&rec.Author, &rec.Revision,
				&rec.CreatedAt, &rec.UpdatedAt, &rec.DeletedAt); err != nil {
				return db.RecurrenceExport{}, scanError("recurrence", err)
			}
			if !json.Valid([]byte(labels)) {
				return db.RecurrenceExport{}, invalidJSONErr("recurrence", rec.ID, "template_labels")
			}
			if !json.Valid([]byte(metadata)) {
				return db.RecurrenceExport{}, invalidJSONErr("recurrence", rec.ID, "template_metadata")
			}
			rec.TemplateLabels = json.RawMessage(labels)
			rec.TemplateMetadata = json.RawMessage(metadata)
			return rec, nil
		})
}

// ExportLinks streams links ordered by id, scoped to f.ProjectID when set.
// Soft-deleted endpoints (either side) drop the link unless IncludeDeleted is true.
func (d *Store) ExportLinks(ctx context.Context, f db.ExportFilter) iter.Seq2[db.LinkExport, error] {
	query := `SELECT links.id, links.project_id, links.from_issue_id, links.from_issue_uid,
	                 links.to_issue_id, links.to_issue_uid,
	                 links.type, links.author, CAST(links.created_at AS TEXT)
	          FROM links
	          JOIN issues AS from_issues ON from_issues.id = links.from_issue_id
	          JOIN issues AS to_issues ON to_issues.id = links.to_issue_id`
	var clauses []string
	var args []any
	if f.ProjectID != nil {
		clauses = append(clauses, `links.project_id = ?`)
		args = append(args, *f.ProjectID)
	}
	if !f.IncludeDeleted {
		clauses = append(clauses, `from_issues.deleted_at IS NULL`, `to_issues.deleted_at IS NULL`)
	}
	query += joinClauses(clauses) + ` ORDER BY links.id ASC`
	return streamRows(ctx, d.readQ, "links", query, args,
		func(rows *sql.Rows) (db.LinkExport, error) {
			var rec db.LinkExport
			if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.FromIssueID, &rec.FromIssueUID,
				&rec.ToIssueID, &rec.ToIssueUID, &rec.Type, &rec.Author, &rec.CreatedAt); err != nil {
				return db.LinkExport{}, scanError("link", err)
			}
			return rec, nil
		})
}

// ExportProjectAliases streams aliases ordered by id, scoped to f.ProjectID
// when set. There is no soft-delete clause.
func (d *Store) ExportProjectAliases(ctx context.Context, f db.ExportFilter) iter.Seq2[db.AliasExport, error] {
	query := `SELECT id, project_id, alias_identity, alias_kind, root_path,
	                 CAST(created_at AS TEXT), CAST(last_seen_at AS TEXT)
	          FROM project_aliases`
	query, args := withProjectIDFilter(query, f, "project_id")
	query += ` ORDER BY id ASC`
	return streamRows(ctx, d.readQ, "project_aliases", query, args,
		func(rows *sql.Rows) (db.AliasExport, error) {
			var rec db.AliasExport
			if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.AliasIdentity, &rec.AliasKind,
				&rec.RootPath, &rec.CreatedAt, &rec.LastSeenAt); err != nil {
				return db.AliasExport{}, scanError("project_alias", err)
			}
			return rec, nil
		})
}

// ExportComments streams comments ordered by id, scoped via the parent issue
// (project + soft-delete rides on issues).
func (d *Store) ExportComments(ctx context.Context, f db.ExportFilter) iter.Seq2[db.CommentExport, error] {
	query := `SELECT comments.id, comments.uid, comments.issue_id, comments.author, comments.body, CAST(comments.created_at AS TEXT)
	          FROM comments
	          JOIN issues ON issues.id = comments.issue_id` +
		exportWhere("issues", f) + ` ORDER BY comments.id ASC`
	return streamRows(ctx, d.readQ, "comments", query, exportArgs(f),
		func(rows *sql.Rows) (db.CommentExport, error) {
			var rec db.CommentExport
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.IssueID, &rec.Author, &rec.Body, &rec.CreatedAt); err != nil {
				return db.CommentExport{}, scanError("comment", err)
			}
			return rec, nil
		})
}

// ExportIssueLabels streams labels ordered by (issue_id, label), scoped via
// the parent issue.
func (d *Store) ExportIssueLabels(ctx context.Context, f db.ExportFilter) iter.Seq2[db.IssueLabelExport, error] {
	query := `SELECT issue_labels.issue_id, issue_labels.label, issue_labels.author, CAST(issue_labels.created_at AS TEXT)
	          FROM issue_labels
	          JOIN issues ON issues.id = issue_labels.issue_id` +
		exportWhere("issues", f) + ` ORDER BY issue_labels.issue_id ASC, issue_labels.label ASC`
	return streamRows(ctx, d.readQ, "issue_labels", query, exportArgs(f),
		func(rows *sql.Rows) (db.IssueLabelExport, error) {
			var rec db.IssueLabelExport
			if err := rows.Scan(&rec.IssueID, &rec.Label, &rec.Author, &rec.CreatedAt); err != nil {
				return db.IssueLabelExport{}, scanError("issue_label", err)
			}
			return rec, nil
		})
}

// ExportImportMappings streams import_mappings ordered by id, scoped to
// f.ProjectID when set. Soft-deleted mappings (via the underlying issue/link)
// drop unless IncludeDeleted is true.
func (d *Store) ExportImportMappings(ctx context.Context, f db.ExportFilter) iter.Seq2[db.ImportMappingExport, error] {
	query := `SELECT id, source, external_id, object_type, project_id, issue_id, comment_id, link_id, label,
	                 CAST(source_updated_at AS TEXT), CAST(imported_at AS TEXT)
	          FROM import_mappings`
	var clauses []string
	var args []any
	if f.ProjectID != nil {
		clauses = append(clauses, `project_id = ?`)
		args = append(args, *f.ProjectID)
	}
	if !f.IncludeDeleted {
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
	query += joinClauses(clauses) + ` ORDER BY id ASC`
	return streamRows(ctx, d.readQ, "import_mappings", query, args,
		func(rows *sql.Rows) (db.ImportMappingExport, error) {
			var rec db.ImportMappingExport
			if err := rows.Scan(&rec.ID, &rec.Source, &rec.ExternalID, &rec.ObjectType, &rec.ProjectID,
				&rec.IssueID, &rec.CommentID, &rec.LinkID, &rec.Label, &rec.SourceUpdatedAt, &rec.ImportedAt); err != nil {
				return db.ImportMappingExport{}, scanError("import_mapping", err)
			}
			return rec, nil
		})
}

// ExportFederationBindings streams federation_bindings ordered by project_id,
// scoped to f.ProjectID when set.
func (d *Store) ExportFederationBindings(ctx context.Context, f db.ExportFilter) iter.Seq2[db.FederationBindingExport, error] {
	query := `SELECT project_id, role, hub_url, hub_project_id, hub_project_uid,
	                 replay_horizon_event_id, pull_cursor_event_id, push_enabled,
	                 push_cursor_event_id, enabled,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT),
	                 CAST(last_sync_at AS TEXT)
	          FROM federation_bindings`
	query, args := withProjectIDFilter(query, f, "project_id")
	query += ` ORDER BY project_id ASC`
	return streamRows(ctx, d.readQ, "federation_bindings", query, args,
		func(rows *sql.Rows) (db.FederationBindingExport, error) {
			var rec db.FederationBindingExport
			var enabled, pushEnabled int
			if err := rows.Scan(&rec.ProjectID, &rec.Role, &rec.HubURL, &rec.HubProjectID,
				&rec.HubProjectUID, &rec.ReplayHorizonEventID, &rec.PullCursorEventID,
				&pushEnabled, &rec.PushCursorEventID, &enabled, &rec.CreatedAt,
				&rec.UpdatedAt, &rec.LastSyncAt); err != nil {
				return db.FederationBindingExport{}, scanError("federation_binding", err)
			}
			rec.PushEnabled = pushEnabled == 1
			rec.Enabled = enabled == 1
			return rec, nil
		})
}

// ExportFederationSyncStatus streams federation_sync_status ordered by project_id.
func (d *Store) ExportFederationSyncStatus(ctx context.Context, f db.ExportFilter) iter.Seq2[db.FederationSyncStatusExport, error] {
	query := `SELECT project_id,
	                 CAST(last_pull_started_at AS TEXT), CAST(last_pull_success_at AS TEXT),
	                 CAST(last_push_started_at AS TEXT), CAST(last_push_success_at AS TEXT),
	                 CAST(last_error_at AS TEXT), last_error, CAST(last_reset_at AS TEXT)
	            FROM federation_sync_status`
	query, args := withProjectIDFilter(query, f, "project_id")
	query += ` ORDER BY project_id ASC`
	return streamRows(ctx, d.readQ, "federation_sync_status", query, args,
		func(rows *sql.Rows) (db.FederationSyncStatusExport, error) {
			var rec db.FederationSyncStatusExport
			if err := rows.Scan(&rec.ProjectID, &rec.LastPullStartedAt, &rec.LastPullSuccessAt,
				&rec.LastPushStartedAt, &rec.LastPushSuccessAt, &rec.LastErrorAt,
				&rec.LastError, &rec.LastResetAt); err != nil {
				return db.FederationSyncStatusExport{}, scanError("federation_sync_status", err)
			}
			return rec, nil
		})
}

// ExportFederationQuarantine streams federation_quarantine rows ordered by id.
func (d *Store) ExportFederationQuarantine(ctx context.Context, f db.ExportFilter) iter.Seq2[db.FederationQuarantineExport, error] {
	query := `SELECT id, project_id, direction, first_event_id, last_event_id,
	                 event_uids, error, CAST(created_at AS TEXT),
	                 CAST(skipped_at AS TEXT), skipped_by, skip_reason
	            FROM federation_quarantine`
	query, args := withProjectIDFilter(query, f, "project_id")
	query += ` ORDER BY id ASC`
	return streamRows(ctx, d.readQ, "federation_quarantine", query, args,
		func(rows *sql.Rows) (db.FederationQuarantineExport, error) {
			var rec db.FederationQuarantineExport
			var eventUIDs string
			if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.Direction, &rec.FirstEventID,
				&rec.LastEventID, &eventUIDs, &rec.Error, &rec.CreatedAt,
				&rec.SkippedAt, &rec.SkippedBy, &rec.SkipReason); err != nil {
				return db.FederationQuarantineExport{}, scanError("federation_quarantine", err)
			}
			if !json.Valid([]byte(eventUIDs)) {
				return db.FederationQuarantineExport{}, fmt.Errorf("federation quarantine %d event_uids is invalid JSON", rec.ID)
			}
			rec.EventUIDs = json.RawMessage(eventUIDs)
			return rec, nil
		})
}

// ExportFederationEnrollments streams federation_enrollments rows ordered by id.
func (d *Store) ExportFederationEnrollments(ctx context.Context, f db.ExportFilter) iter.Seq2[db.FederationEnrollmentExport, error] {
	query := `SELECT id, token_hash, spoke_instance_uid, project_id, capabilities,
	                 CAST(created_at AS TEXT), CAST(updated_at AS TEXT), CAST(revoked_at AS TEXT)
	          FROM federation_enrollments`
	query, args := withProjectIDFilter(query, f, "project_id")
	query += ` ORDER BY id ASC`
	return streamRows(ctx, d.readQ, "federation_enrollments", query, args,
		func(rows *sql.Rows) (db.FederationEnrollmentExport, error) {
			var rec db.FederationEnrollmentExport
			if err := rows.Scan(&rec.ID, &rec.TokenHash, &rec.SpokeInstanceUID, &rec.ProjectID,
				&rec.Capabilities, &rec.CreatedAt, &rec.UpdatedAt, &rec.RevokedAt); err != nil {
				return db.FederationEnrollmentExport{}, scanError("federation_enrollment", err)
			}
			return rec, nil
		})
}

// ExportIssueClaims streams issue_claims rows ordered by id, scoped via the
// parent issue (project + soft-delete rides on issues).
func (d *Store) ExportIssueClaims(ctx context.Context, f db.ExportFilter) iter.Seq2[db.IssueClaimExport, error] {
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
	          JOIN issues ON issues.id = issue_claims.issue_id` +
		exportWhere("issues", f) + ` ORDER BY issue_claims.id ASC`
	return streamRows(ctx, d.readQ, "issue_claims", query, exportArgs(f),
		func(rows *sql.Rows) (db.IssueClaimExport, error) {
			var rec db.IssueClaimExport
			if err := rows.Scan(&rec.ID, &rec.ClaimUID, &rec.ProjectID, &rec.IssueID,
				&rec.IssueUID, &rec.Holder, &rec.HolderInstanceUID, &rec.ClientKind,
				&rec.Purpose, &rec.ClaimKind, &rec.AcquiredAt, &rec.ExpiresAt,
				&rec.ReleasedAt, &rec.ReleaseReason, &rec.Revision, &rec.UpdatedAt); err != nil {
				return db.IssueClaimExport{}, scanError("issue_claim", err)
			}
			return rec, nil
		})
}

// ExportPendingClaimRequests streams pending_claim_requests rows ordered by id,
// scoped via the parent issue.
func (d *Store) ExportPendingClaimRequests(ctx context.Context, f db.ExportFilter) iter.Seq2[db.PendingClaimRequestExport, error] {
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
	          JOIN issues ON issues.id = pending_claim_requests.issue_id` +
		exportWhere("issues", f) + ` ORDER BY pending_claim_requests.id ASC`
	return streamRows(ctx, d.readQ, "pending_claim_requests", query, exportArgs(f),
		func(rows *sql.Rows) (db.PendingClaimRequestExport, error) {
			var rec db.PendingClaimRequestExport
			if err := rows.Scan(&rec.ID, &rec.RequestUID, &rec.ProjectID, &rec.IssueID,
				&rec.IssueUID, &rec.Holder, &rec.HolderInstanceUID, &rec.ClientKind,
				&rec.ClaimKind, &rec.TTLSeconds, &rec.Purpose, &rec.RequestedAt, &rec.LastAttemptAt, &rec.LastError,
				&rec.RejectedAt, &rec.ResolvedAt); err != nil {
				return db.PendingClaimRequestExport{}, scanError("pending_claim_request", err)
			}
			return rec, nil
		})
}

// ExportSequences streams sqlite_sequence rows ordered by name. SQLite-only.
func (d *Store) ExportSequences(ctx context.Context) iter.Seq2[db.SequenceExport, error] {
	return streamRows(ctx, d.readQ, "sqlite_sequence",
		`SELECT name, seq FROM sqlite_sequence ORDER BY name ASC`, nil,
		func(rows *sql.Rows) (db.SequenceExport, error) {
			var rec db.SequenceExport
			if err := rows.Scan(&rec.Name, &rec.Seq); err != nil {
				return db.SequenceExport{}, scanError("sqlite_sequence", err)
			}
			return rec, nil
		})
}

// ExportPurgeLog streams purge_log rows ordered by id, scoped to f.ProjectID
// when set. There is no soft-delete clause on purge_log.
func (d *Store) ExportPurgeLog(ctx context.Context, f db.ExportFilter) iter.Seq2[db.PurgeLogExport, error] {
	query := `SELECT purge_log.id, purge_log.uid, purge_log.origin_instance_uid, purge_log.project_id, purged_issue_id, issue_uid, project_uid,
	                 purge_log.project_name, short_id, issue_title,
	                 issue_author, comment_count, link_count, label_count, event_count,
	                 events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
	                 actor, reason, CAST(purged_at AS TEXT)
	          FROM purge_log`
	query, args := withProjectIDFilter(query, f, "purge_log.project_id")
	query += ` ORDER BY purge_log.id ASC`
	return streamRows(ctx, d.readQ, "purge_log", query, args,
		func(rows *sql.Rows) (db.PurgeLogExport, error) {
			var rec db.PurgeLogExport
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.PurgedIssueID, &rec.IssueUID,
				&rec.ProjectUID, &rec.ProjectName, &rec.ShortID, &rec.IssueTitle, &rec.IssueAuthor, &rec.CommentCount,
				&rec.LinkCount, &rec.LabelCount, &rec.EventCount, &rec.EventsDeletedMinID,
				&rec.EventsDeletedMaxID, &rec.PurgeResetAfterEventID, &rec.Actor, &rec.Reason,
				&rec.PurgedAt); err != nil {
				return db.PurgeLogExport{}, scanError("purge_log", err)
			}
			return rec, nil
		})
}

// ExportEvents streams events ordered by id, reproducing the orphan filter and
// related-id scrub from the v10 jsonl export.
func (d *Store) ExportEvents(ctx context.Context, f db.ExportFilter) iter.Seq2[db.EventExport, error] {
	// Scrub related_issue_id/_uid when the peer is missing entirely (any
	// event type, either id-keyed or uid-keyed) OR, on live-only export,
	// when an issue.links_changed peer is soft-deleted (kata#1 history-
	// preservation rule). Peer-missing must be checked first so
	// `peer.deleted_at` doesn't dereference a NULL row. The peer JOIN
	// matches by id when present, and falls back to uid for federation-
	// inserted events that carry only related_issue_uid.
	scrubCondition := `(peer.id IS NULL AND (events.related_issue_id IS NOT NULL OR events.related_issue_uid IS NOT NULL))`
	if !f.IncludeDeleted {
		scrubCondition += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
	}
	relatedIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_id END`
	relatedUIDExpr := `CASE WHEN ` + scrubCondition + ` THEN NULL ELSE events.related_issue_uid END`
	subjectLiveClause := `((events.issue_id IS NULL AND events.issue_uid IS NULL) OR subject_issue.id IS NOT NULL)`
	if !f.IncludeDeleted {
		subjectLiveClause = `((events.issue_id IS NULL AND events.issue_uid IS NULL) OR (subject_issue.id IS NOT NULL AND subject_issue.deleted_at IS NULL))`
	}
	query := `SELECT events.id, events.uid, events.origin_instance_uid, events.project_id, export_project.uid, events.project_name, events.issue_id, events.issue_uid,
	                 ` + relatedIDExpr + `, ` + relatedUIDExpr + `,
	                 events.type, events.actor, events.payload, events.hlc_physical_ms, events.hlc_counter, events.content_hash,
	                 CAST(events.created_at AS TEXT)
	          FROM events
	          JOIN projects export_project ON export_project.id = events.project_id
	          LEFT JOIN issues subject_issue ON subject_issue.project_id = events.project_id
	               AND (subject_issue.id = events.issue_id OR (events.issue_id IS NULL AND events.issue_uid IS NOT NULL AND subject_issue.uid = events.issue_uid))
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id
	               OR (events.related_issue_id IS NULL AND events.related_issue_uid IS NOT NULL AND peer.uid = events.related_issue_uid)`

	clauses := []string{subjectLiveClause}
	var args []any
	if f.ProjectID != nil {
		clauses = append(clauses, `events.project_id = ?`)
		args = append(args, *f.ProjectID)
	}
	if !f.IncludeDeleted {
		// Drop events whose related peer is soft-deleted, by id or by uid
		// (the latter covers federation-inserted UID-only events).
		// issue.links_changed events are exempt: they retain their peer
		// reference for history reconstruction.
		clauses = append(clauses,
			`(events.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues WHERE issues.id = events.issue_id AND issues.deleted_at IS NULL))`,
			`(events.type = 'issue.links_changed'
			  OR ((events.related_issue_id IS NULL OR NOT EXISTS (SELECT 1 FROM issues WHERE issues.id = events.related_issue_id AND issues.deleted_at IS NOT NULL))
			      AND (events.related_issue_uid IS NULL OR NOT EXISTS (SELECT 1 FROM issues WHERE issues.uid = events.related_issue_uid AND issues.deleted_at IS NOT NULL))))`,
		)
	}
	query += joinClauses(clauses) + ` ORDER BY events.id ASC`

	return streamRows(ctx, d.readQ, "events", query, args,
		func(rows *sql.Rows) (db.EventExport, error) {
			var rec db.EventExport
			var payload string
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.ProjectUID, &rec.ProjectName, &rec.IssueID,
				&rec.IssueUID, &rec.RelatedIssueID, &rec.RelatedIssueUID,
				&rec.Type, &rec.Actor, &payload, &rec.HLCPhysicalMS, &rec.HLCCounter, &rec.ContentHash, &rec.CreatedAt); err != nil {
				return db.EventExport{}, scanError("event", err)
			}
			if !json.Valid([]byte(payload)) {
				return db.EventExport{}, invalidJSONErr("event", rec.ID, "payload")
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
				return db.EventExport{}, fmt.Errorf("event %d content hash: %w", rec.ID, err)
			}
			rec.ContentHash = contentHash
			return rec, nil
		})
}

// ExportProjects streams projects ordered by id. Archived (soft-deleted)
// projects are always included; ExportFilter.IncludeDeleted does not apply.
func (d *Store) ExportProjects(ctx context.Context, f db.ExportFilter) iter.Seq2[db.ProjectExport, error] {
	query := `SELECT id, uid, name, CAST(created_at AS TEXT),
	                 CAST(deleted_at AS TEXT), metadata, revision FROM projects`
	query, args := withProjectIDFilter(query, f, "id")
	query += ` ORDER BY id ASC`
	return streamRows(ctx, d.readQ, "projects", query, args,
		func(rows *sql.Rows) (db.ProjectExport, error) {
			var rec db.ProjectExport
			var metadata string
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.Name, &rec.CreatedAt, &rec.DeletedAt,
				&metadata, &rec.Revision); err != nil {
				return db.ProjectExport{}, scanError("project", err)
			}
			if !json.Valid([]byte(metadata)) {
				return db.ProjectExport{}, invalidJSONErr("project", rec.ID, "metadata")
			}
			rec.Metadata = json.RawMessage(metadata)
			return rec, nil
		})
}
