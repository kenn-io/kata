package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func pgReplayMeta(ctx context.Context, tx *sql.Tx, meta *db.MetaKV, opts db.ImportOptions) error {
	if meta.Key == "export_version" || meta.Key == "schema_version" {
		return nil
	}
	if meta.Key == "instance_uid" && opts.NewInstance {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, meta.Key, meta.Value)
	return pgReplayError(db.ImportKindMeta, err)
}

func pgReplayProject(ctx context.Context, tx *sql.Tx, project *db.ProjectExport) error {
	if err := config.ValidateProjectName(project.Name); err != nil {
		return pgReplayError(db.ImportKindProject,
			fmt.Errorf("project %d name %q: %w", project.ID, project.Name, err))
	}
	name := project.Name
	if project.UID != db.SystemProjectUID || project.Name != db.SystemProjectName {
		var renamed bool
		var err error
		name, renamed, err = pgReplayUniqueProjectName(ctx, tx, project.ID, project.Name)
		if err != nil {
			return err
		}
		if renamed {
			fmt.Fprintf(os.Stderr, "note: project #%d renamed from %q to %q during import\n",
				project.ID, project.Name, name)
		}
	}
	metadata := project.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	revision := project.Revision
	if revision == 0 {
		revision = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO projects(id,uid,name,created_at,deleted_at,metadata,revision)
OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7)`,
		project.ID, project.UID, name, project.CreatedAt, project.DeletedAt, string(metadata), revision)
	return pgReplayError(db.ImportKindProject, err)
}

func pgReplayUniqueProjectName(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	requested string,
) (string, bool, error) {
	original := requested
	name := requested
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("project-%d", projectID)
		original = name
	}
	if name == db.SystemProjectName {
		name = db.SystemProjectName + "-2"
	}
	for suffix := 1; ; suffix++ {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM projects WHERE name=$1)`, name).Scan(&exists); err != nil {
			return "", false, fmt.Errorf("check project name collision: %w", mapSQLError(err, nil))
		}
		if !exists {
			return name, name != original, nil
		}
		name = fmt.Sprintf("%s-%d", original, suffix+1)
	}
}

func pgReplayAlias(ctx context.Context, tx *sql.Tx, alias *db.AliasExport) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_aliases(
id,project_id,alias_identity,alias_kind,created_at
) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5)`, alias.ID, alias.ProjectID,
		alias.AliasIdentity, alias.AliasKind, alias.CreatedAt)
	return pgReplayError(db.ImportKindProjectAlias, err)
}

func pgReplayIssueSyncBinding(
	ctx context.Context,
	tx *sql.Tx,
	binding *db.IssueSyncBindingExport,
	preserveEnabled bool,
) error {
	enabled := preserveEnabled && binding.Enabled
	configJSON := binding.Config
	if len(configJSON) == 0 {
		configJSON = json.RawMessage(`{}`)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO issue_sync_bindings(
id,project_id,provider,source_key,remote_id,display_name,config_json,enabled,
interval_seconds,last_cursor_at,created_at,updated_at
) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		binding.ID, binding.ProjectID, binding.Provider, binding.SourceKey, binding.RemoteID,
		binding.DisplayName, string(configJSON), pgReplayBoolInt(enabled), binding.IntervalSeconds,
		binding.LastCursorAt, binding.CreatedAt, binding.UpdatedAt)
	return pgReplayError(db.ImportKindIssueSyncBinding, err)
}

func pgReplayIssueSyncStatus(ctx context.Context, tx *sql.Tx, status *db.IssueSyncStatusExport) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO issue_sync_status(
binding_id,project_id,sync_started_at,last_attempt_at,last_success_at,last_error_at,last_error,
last_created,last_updated,last_unchanged,last_comments
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, status.BindingID, status.ProjectID,
		status.SyncStartedAt, status.LastAttemptAt, status.LastSuccessAt, status.LastErrorAt,
		status.LastError, status.LastCreated, status.LastUpdated, status.LastUnchanged, status.LastComments)
	return pgReplayError(db.ImportKindIssueSyncStatus, err)
}

func pgReplayRecurrence(ctx context.Context, tx *sql.Tx, recurrence *db.RecurrenceExport) error {
	labels := recurrence.TemplateLabels
	if len(labels) == 0 {
		labels = json.RawMessage(`[]`)
	}
	metadata := recurrence.TemplateMetadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO recurrences(
id,uid,project_id,rrule,dtstart,timezone,template_title,template_body,template_owner,
template_priority,template_labels,template_metadata,next_occurrence_key,last_materialized_uid,
author,revision,created_at,updated_at,deleted_at
) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		recurrence.ID, recurrence.UID, recurrence.ProjectID, recurrence.RRule, recurrence.DTStart,
		recurrence.Timezone, recurrence.TemplateTitle, recurrence.TemplateBody, recurrence.TemplateOwner,
		recurrence.TemplatePriority, string(labels), string(metadata), recurrence.NextOccurrenceKey,
		recurrence.LastMaterializedUID, recurrence.Author, recurrence.Revision, recurrence.CreatedAt,
		recurrence.UpdatedAt, recurrence.DeletedAt)
	return pgReplayError(db.ImportKindRecurrence, err)
}

func pgReplayIssue(ctx context.Context, tx *sql.Tx, issue *db.IssueExport) error {
	if issue.ShortID == "" {
		return fmt.Errorf("import issue %d: missing short_id (older envelopes must go through cutover)", issue.ID)
	}
	metadata := issue.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	revision := issue.Revision
	if revision == 0 {
		revision = 1
	}
	if issue.OccurrenceKey != nil && issue.RecurrenceUID == nil && issue.RecurrenceID == nil {
		return fmt.Errorf("import issue %d (uid=%s): occurrence_key set without recurrence_uid", issue.ID, issue.UID)
	}
	recurrenceID := issue.RecurrenceID
	if issue.RecurrenceUID != nil {
		var resolved int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM recurrences WHERE uid=$1`, *issue.RecurrenceUID).Scan(&resolved)
		if err != nil {
			return fmt.Errorf("import issue %d: recurrence_uid %q not found: %w",
				issue.ID, *issue.RecurrenceUID, mapSQLError(err, nil))
		}
		if issue.RecurrenceID != nil && *issue.RecurrenceID != resolved {
			return fmt.Errorf(
				"import issue %d: recurrence_uid %q resolves to id %d, but record carries recurrence_id %d",
				issue.ID, *issue.RecurrenceUID, resolved, *issue.RecurrenceID,
			)
		}
		recurrenceID = &resolved
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO issues(
id,uid,project_id,short_id,title,body,status,closed_reason,owner,priority,author,created_at,
updated_at,closed_at,deleted_at,metadata,revision,content_revision,recurrence_id,occurrence_key
) OVERRIDING SYSTEM VALUE VALUES(
$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20
)`, issue.ID, issue.UID, issue.ProjectID, issue.ShortID, issue.Title, issue.Body, issue.Status,
		issue.ClosedReason, issue.Owner, issue.Priority, issue.Author, issue.CreatedAt, issue.UpdatedAt,
		issue.ClosedAt, issue.DeletedAt, string(metadata), revision, issue.ContentRevision,
		recurrenceID, issue.OccurrenceKey)
	return pgReplayError(db.ImportKindIssue, err)
}

func pgReplayComment(ctx context.Context, tx *sql.Tx, comment *db.CommentExport) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO comments(id,uid,issue_id,author,body,created_at)
OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6)`, comment.ID, comment.UID,
		comment.IssueID, comment.Author, comment.Body, comment.CreatedAt)
	return pgReplayError(db.ImportKindComment, err)
}

func pgReplayLabel(ctx context.Context, tx *sql.Tx, label *db.IssueLabelExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id,label,author,created_at) VALUES($1,$2,$3,$4)`,
		label.IssueID, label.Label, label.Author, label.CreatedAt)
	return pgReplayError(db.ImportKindIssueLabel, err)
}

func pgReplayLink(ctx context.Context, tx *sql.Tx, link *db.LinkExport) (replayLinkSkip, error) {
	var present int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues WHERE id=ANY($1::bigint[])`,
		[]int64{link.FromIssueID, link.ToIssueID}).Scan(&present); err != nil {
		return replayLinkInserted, pgReplayError(db.ImportKindLink, err)
	}
	if present != 2 {
		return replayLinkMissingPeer, nil
	}
	var duplicate bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM links WHERE type=$1 AND (
(from_issue_uid=$2 AND to_issue_uid=$3) OR
(type='related' AND from_issue_uid=$3 AND to_issue_uid=$2)
))`, link.Type, link.FromIssueUID, link.ToIssueUID).Scan(&duplicate)
	if err != nil {
		return replayLinkInserted, pgReplayError(db.ImportKindLink, err)
	}
	if duplicate {
		return replayLinkDuplicate, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO links(
id,from_issue_id,from_issue_uid,to_issue_id,to_issue_uid,type,author,created_at
) OVERRIDING SYSTEM VALUE VALUES(
$1,$2,COALESCE(NULLIF($3,''),(SELECT uid FROM issues WHERE id=$2)),
$4,COALESCE(NULLIF($5,''),(SELECT uid FROM issues WHERE id=$4)),$6,$7,$8
)`, link.ID, link.FromIssueID, link.FromIssueUID, link.ToIssueID, link.ToIssueUID,
		link.Type, link.Author, link.CreatedAt)
	return replayLinkInserted, pgReplayError(db.ImportKindLink, err)
}

func pgReplayImportMapping(
	ctx context.Context,
	tx *sql.Tx,
	mapping *db.ImportMappingExport,
	skippedLinkIDs map[int64]struct{},
) (replayLinkSkip, error) {
	if mapping.LinkID != nil {
		if _, skipped := skippedLinkIDs[*mapping.LinkID]; skipped {
			return replayLinkMapping, nil
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO import_mappings(
id,source,external_id,object_type,project_id,issue_id,comment_id,link_id,label,source_updated_at,imported_at
) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		mapping.ID, mapping.Source, mapping.ExternalID, mapping.ObjectType, mapping.ProjectID,
		mapping.IssueID, mapping.CommentID, mapping.LinkID, mapping.Label, mapping.SourceUpdatedAt,
		mapping.ImportedAt)
	return replayLinkInserted, pgReplayError(db.ImportKindImportMapping, err)
}

func pgReplayIssueClaim(ctx context.Context, tx *sql.Tx, claim *db.IssueClaimExport) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO issue_claims(
id,claim_uid,project_id,issue_id,issue_uid,holder,holder_instance_uid,client_kind,purpose,
claim_kind,acquired_at,expires_at,released_at,release_reason,revision,updated_at
) OVERRIDING SYSTEM VALUE VALUES(
$1,$2,$3,$4,COALESCE(NULLIF($5,''),(SELECT uid FROM issues WHERE id=$4)),
$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16
)`, claim.ID, claim.ClaimUID, claim.ProjectID, claim.IssueID, claim.IssueUID, claim.Holder,
		claim.HolderInstanceUID, claim.ClientKind, claim.Purpose, claim.ClaimKind, claim.AcquiredAt,
		claim.ExpiresAt, claim.ReleasedAt, claim.ReleaseReason, claim.Revision, claim.UpdatedAt)
	return pgReplayError(db.ImportKindIssueClaim, err)
}

func pgReplayPendingClaim(
	ctx context.Context,
	tx *sql.Tx,
	request *db.PendingClaimRequestExport,
	opts db.ImportOptions,
) error {
	if opts.DedupeLegacyActivePendingClaims && request.RejectedAt == nil && request.ResolvedAt == nil {
		skip, err := pgReplaySkipLegacyPendingClaim(ctx, tx, request)
		if err != nil {
			return pgReplayError(db.ImportKindPendingClaimRequest, err)
		}
		if skip {
			return nil
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO pending_claim_requests(
id,request_uid,project_id,issue_id,issue_uid,holder,holder_instance_uid,client_kind,
claim_kind,ttl_seconds,purpose,requested_at,last_attempt_at,last_error,rejected_at,resolved_at
) OVERRIDING SYSTEM VALUE VALUES(
$1,$2,$3,$4,COALESCE(NULLIF($5,''),(SELECT uid FROM issues WHERE id=$4)),
$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16
)`, request.ID, request.RequestUID, request.ProjectID, request.IssueID, request.IssueUID,
		request.Holder, request.HolderInstanceUID, request.ClientKind, request.ClaimKind,
		request.TTLSeconds, request.Purpose, request.RequestedAt, request.LastAttemptAt,
		request.LastError, request.RejectedAt, request.ResolvedAt)
	return pgReplayError(db.ImportKindPendingClaimRequest, err)
}

func pgReplaySkipLegacyPendingClaim(
	ctx context.Context,
	tx *sql.Tx,
	request *db.PendingClaimRequestExport,
) (bool, error) {
	var existingID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM pending_claim_requests
WHERE issue_uid=COALESCE(NULLIF($1,''),(SELECT uid FROM issues WHERE id=$2))
AND holder_instance_uid=$3 AND holder=$4 AND client_kind=$5
AND rejected_at IS NULL AND resolved_at IS NULL
ORDER BY requested_at ASC,id ASC LIMIT 1`, request.IssueUID, request.IssueID,
		request.HolderInstanceUID, request.Holder, request.ClientKind).Scan(&existingID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}
