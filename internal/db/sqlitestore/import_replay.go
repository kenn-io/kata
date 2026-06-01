package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// ImportReplay performs the entire JSONL import as one atomic transaction.
// recs must already be normalized to the current shape (jsonl does cutover +
// version fills before calling). The flow:
//  1. validate every record up front so a malformed batch never mutates the DB.
//  2. open a tx, defer FK enforcement so out-of-order child rows don't fail.
//  3. delete the auto-created system project so the imported one (if present)
//     wins, then dispatch every record in slice order.
//  4. ensure the system project exists, replay the api_tokens projection from
//     the imported events, stamp the current schema_version, reconcile
//     sqlite_sequence, validate FKs + integrity, commit.
//  5. refresh the cached instance UID — default mode overwrites it with the
//     source's.
func (d *Store) ImportReplay(ctx context.Context, recs []db.ImportRecord, opts db.ImportOptions) error {
	for i, r := range recs {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("import record %d: %w", i, err)
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return fmt.Errorf("defer foreign keys: %w", err)
	}
	if err := removeAutoSystemProject(ctx, tx); err != nil {
		return err
	}

	for _, r := range recs {
		if err := importRecord(ctx, tx, r, opts); err != nil {
			return err
		}
	}
	if err := ensureSystemProject(ctx, tx); err != nil {
		return err
	}
	if err := replayAPITokenProjection(ctx, tx); err != nil {
		return err
	}
	if err := recordImportSchemaVersion(ctx, tx); err != nil {
		return err
	}
	if err := reconcileSequences(ctx, tx); err != nil {
		return err
	}
	if err := validateBeforeCommit(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import: %w", err)
	}
	return d.RefreshInstanceUID(ctx)
}

func importRecord(ctx context.Context, tx *sql.Tx, r db.ImportRecord, opts db.ImportOptions) error {
	switch r.Kind {
	case db.ImportKindMeta:
		return importMeta(ctx, tx, r.Meta, opts)
	case db.ImportKindProject:
		return importProject(ctx, tx, r.Project)
	case db.ImportKindProjectAlias:
		return importAlias(ctx, tx, r.Alias)
	case db.ImportKindRecurrence:
		return importRecurrence(ctx, tx, r.Recurrence)
	case db.ImportKindIssue:
		return importIssue(ctx, tx, r.Issue)
	case db.ImportKindComment:
		return importComment(ctx, tx, r.Comment)
	case db.ImportKindIssueLabel:
		return importLabel(ctx, tx, r.Label)
	case db.ImportKindLink:
		return importLink(ctx, tx, r.Link)
	case db.ImportKindImportMapping:
		return importMapping(ctx, tx, r.ImportMapping)
	case db.ImportKindFederationBinding:
		return importFederationBinding(ctx, tx, r.FederationBinding)
	case db.ImportKindFederationSyncStatus:
		return importFederationSyncStatus(ctx, tx, r.FederationSyncStatus)
	case db.ImportKindFederationQuarantine:
		return importFederationQuarantine(ctx, tx, r.FederationQuarantine)
	case db.ImportKindFederationEnrollment:
		return importFederationEnrollment(ctx, tx, r.FederationEnrollment)
	case db.ImportKindIssueClaim:
		return importIssueClaim(ctx, tx, r.IssueClaim)
	case db.ImportKindPendingClaimRequest:
		return importPendingClaimRequest(ctx, tx, r.PendingClaimRequest, opts)
	case db.ImportKindEvent:
		return importEvent(ctx, tx, r.Event)
	case db.ImportKindPurgeLog:
		return importPurgeLog(ctx, tx, r.PurgeLog)
	case db.ImportKindSQLiteSequence:
		return upsertSequence(ctx, tx, r.Sequence.Name, r.Sequence.Seq)
	default:
		// Unreachable: validate() already rejected unknown kinds.
		return fmt.Errorf("import: unsupported kind %q", r.Kind)
	}
}

func importMeta(ctx context.Context, tx *sql.Tx, m *db.MetaKV, opts db.ImportOptions) error {
	// Always ignore the source's export_version/schema_version; ImportReplay
	// stamps the current schema_version after the loop (recordImportSchemaVersion).
	if m.Key == "export_version" || m.Key == "schema_version" {
		return nil
	}
	// --new-instance keeps the target's instance_uid (the value db.Open wrote).
	if m.Key == "instance_uid" && opts.NewInstance {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		m.Key, m.Value)
	return wrapImportErr(db.ImportKindMeta, err)
}

func importProject(ctx context.Context, tx *sql.Tx, p *db.ProjectExport) error {
	// The system project is identified by UID+name; preserve it verbatim so the
	// post-loop ensureSystemProject treats it as already present.
	if p.UID == db.SystemProjectUID && p.Name == db.SystemProjectName {
		metadata := p.Metadata
		if len(metadata) == 0 {
			metadata = json.RawMessage(`{}`)
		}
		revision := p.Revision
		if revision == 0 {
			revision = 1
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO projects(id, uid, name, created_at, deleted_at, metadata, revision)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			p.ID, p.UID, p.Name, p.CreatedAt, p.DeletedAt, string(metadata), revision)
		return wrapImportErr(db.ImportKindProject, err)
	}
	original := p.Name
	name, renamed, err := uniqueProjectName(ctx, tx, p.ID, p.Name)
	if err != nil {
		return err
	}
	if renamed {
		fmt.Fprintf(os.Stderr, "note: project #%d renamed from %q to %q during import\n", p.ID, original, name)
	}
	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	revision := p.Revision
	if revision == 0 {
		revision = 1
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO projects(id, uid, name, created_at, deleted_at, metadata, revision)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UID, name, p.CreatedAt, p.DeletedAt, string(metadata), revision)
	return wrapImportErr(db.ImportKindProject, err)
}

func importAlias(ctx context.Context, tx *sql.Tx, a *db.AliasExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO project_aliases(id, project_id, alias_identity, alias_kind, root_path, created_at, last_seen_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ProjectID, a.AliasIdentity, a.AliasKind, a.RootPath, a.CreatedAt, a.LastSeenAt)
	return wrapImportErr(db.ImportKindProjectAlias, err)
}

func importRecurrence(ctx context.Context, tx *sql.Tx, rc *db.RecurrenceExport) error {
	labels := rc.TemplateLabels
	if len(labels) == 0 {
		labels = json.RawMessage(`[]`)
	}
	metadata := rc.TemplateMetadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO recurrences
		   (id, uid, project_id, rrule, dtstart, timezone,
		    template_title, template_body, template_owner, template_priority,
		    template_labels, template_metadata,
		    next_occurrence_key, last_materialized_uid,
		    author, revision, created_at, updated_at, deleted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rc.ID, rc.UID, rc.ProjectID, rc.RRule, rc.DTStart, rc.Timezone,
		rc.TemplateTitle, rc.TemplateBody, rc.TemplateOwner, rc.TemplatePriority,
		string(labels), string(metadata),
		rc.NextOccurrenceKey, rc.LastMaterializedUID,
		rc.Author, rc.Revision, rc.CreatedAt, rc.UpdatedAt, rc.DeletedAt)
	return wrapImportErr(db.ImportKindRecurrence, err)
}

func importIssue(ctx context.Context, tx *sql.Tx, i *db.IssueExport) error {
	if i.ShortID == "" {
		return fmt.Errorf("import issue %d: missing short_id (older envelopes must go through cutover)", i.ID)
	}
	metadata := i.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	revision := i.Revision
	if revision == 0 {
		revision = 1
	}
	if i.OccurrenceKey != nil && i.RecurrenceUID == nil && i.RecurrenceID == nil {
		return fmt.Errorf("import issue %d (uid=%s): occurrence_key set without recurrence_uid", i.ID, i.UID)
	}
	recurrenceID := i.RecurrenceID
	if i.RecurrenceUID != nil {
		var resolvedID int64
		if qErr := tx.QueryRowContext(ctx,
			`SELECT id FROM recurrences WHERE uid = ?`, *i.RecurrenceUID,
		).Scan(&resolvedID); qErr != nil {
			return fmt.Errorf("import issue %d: recurrence_uid %q not found: %w", i.ID, *i.RecurrenceUID, qErr)
		}
		if i.RecurrenceID != nil && *i.RecurrenceID != resolvedID {
			return fmt.Errorf("import issue %d: recurrence_uid %q resolves to id %d, but record carries recurrence_id %d",
				i.ID, *i.RecurrenceUID, resolvedID, *i.RecurrenceID)
		}
		recurrenceID = &resolvedID
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO issues(id, uid, project_id, short_id, title, body, status, closed_reason, owner, priority, author,
		                    created_at, updated_at, closed_at, deleted_at, metadata, revision,
		                    recurrence_id, occurrence_key)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.UID, i.ProjectID, i.ShortID, i.Title, i.Body, i.Status, i.ClosedReason,
		i.Owner, i.Priority, i.Author, i.CreatedAt, i.UpdatedAt, i.ClosedAt, i.DeletedAt,
		string(metadata), revision, recurrenceID, i.OccurrenceKey)
	return wrapImportErr(db.ImportKindIssue, err)
}

func importComment(ctx context.Context, tx *sql.Tx, c *db.CommentExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO comments(id, uid, issue_id, author, body, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		c.ID, c.UID, c.IssueID, c.Author, c.Body, c.CreatedAt)
	return wrapImportErr(db.ImportKindComment, err)
}

func importLabel(ctx context.Context, tx *sql.Tx, l *db.IssueLabelExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO issue_labels(issue_id, label, author, created_at) VALUES(?, ?, ?, ?)`,
		l.IssueID, l.Label, l.Author, l.CreatedAt)
	return wrapImportErr(db.ImportKindIssueLabel, err)
}

func importLink(ctx context.Context, tx *sql.Tx, lk *db.LinkExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO links(id, project_id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author, created_at)
		 VALUES(
		   ?, ?, ?,
		   COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?)),
		   ?,
		   COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?)),
		   ?, ?, ?
		 )`,
		lk.ID, lk.ProjectID, lk.FromIssueID, lk.FromIssueUID, lk.FromIssueID,
		lk.ToIssueID, lk.ToIssueUID, lk.ToIssueID, lk.Type, lk.Author, lk.CreatedAt)
	return wrapImportErr(db.ImportKindLink, err)
}

func importMapping(ctx context.Context, tx *sql.Tx, m *db.ImportMappingExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO import_mappings(id, source, external_id, object_type, project_id, issue_id, comment_id, link_id, label, source_updated_at, imported_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Source, m.ExternalID, m.ObjectType, m.ProjectID, m.IssueID, m.CommentID,
		m.LinkID, m.Label, m.SourceUpdatedAt, m.ImportedAt)
	return wrapImportErr(db.ImportKindImportMapping, err)
}

func importFederationBinding(ctx context.Context, tx *sql.Tx, b *db.FederationBindingExport) error {
	enabled := 0
	if b.Enabled {
		enabled = 1
	}
	pushEnabled := 0
	if b.PushEnabled {
		pushEnabled = 1
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO federation_bindings(
		   project_id, role, hub_url, hub_project_id, hub_project_uid,
		   replay_horizon_event_id, pull_cursor_event_id, push_enabled,
		   push_cursor_event_id, enabled,
		   created_at, updated_at, last_sync_at
		 )
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ProjectID, b.Role, b.HubURL, b.HubProjectID, b.HubProjectUID,
		b.ReplayHorizonEventID, b.PullCursorEventID, pushEnabled,
		b.PushCursorEventID, enabled,
		b.CreatedAt, b.UpdatedAt, b.LastSyncAt)
	return wrapImportErr(db.ImportKindFederationBinding, err)
}

func importFederationSyncStatus(ctx context.Context, tx *sql.Tx, s *db.FederationSyncStatusExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO federation_sync_status(
		   project_id, last_pull_started_at, last_pull_success_at,
		   last_push_started_at, last_push_success_at,
		   last_error_at, last_error, last_reset_at
		 )
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ProjectID, s.LastPullStartedAt, s.LastPullSuccessAt,
		s.LastPushStartedAt, s.LastPushSuccessAt,
		s.LastErrorAt, s.LastError, s.LastResetAt)
	return wrapImportErr(db.ImportKindFederationSyncStatus, err)
}

func importFederationQuarantine(ctx context.Context, tx *sql.Tx, q *db.FederationQuarantineExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO federation_quarantine(
		   id, project_id, direction, first_event_id, last_event_id,
		   event_uids, error, created_at, skipped_at, skipped_by, skip_reason
		 )
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.ProjectID, q.Direction, q.FirstEventID, q.LastEventID,
		string(q.EventUIDs), q.Error, q.CreatedAt, q.SkippedAt, q.SkippedBy, q.SkipReason)
	return wrapImportErr(db.ImportKindFederationQuarantine, err)
}

func importFederationEnrollment(ctx context.Context, tx *sql.Tx, e *db.FederationEnrollmentExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO federation_enrollments(
		   id, token_hash, spoke_instance_uid, project_id, capabilities,
		   created_at, updated_at, revoked_at
		 )
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TokenHash, e.SpokeInstanceUID, e.ProjectID, e.Capabilities,
		e.CreatedAt, e.UpdatedAt, e.RevokedAt)
	return wrapImportErr(db.ImportKindFederationEnrollment, err)
}

func importIssueClaim(ctx context.Context, tx *sql.Tx, c *db.IssueClaimExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO issue_claims(
		   id, claim_uid, project_id, issue_id, issue_uid, holder,
		   holder_instance_uid, client_kind, purpose, claim_kind,
		   acquired_at, expires_at, released_at, release_reason, revision, updated_at
		 )
		 VALUES(
		   ?, ?, ?, ?,
		   COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?)),
		   ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 )`,
		c.ID, c.ClaimUID, c.ProjectID, c.IssueID,
		c.IssueUID, c.IssueID,
		c.Holder, c.HolderInstanceUID, c.ClientKind, c.Purpose,
		c.ClaimKind, c.AcquiredAt, c.ExpiresAt, c.ReleasedAt,
		c.ReleaseReason, c.Revision, c.UpdatedAt)
	return wrapImportErr(db.ImportKindIssueClaim, err)
}

func importPendingClaimRequest(ctx context.Context, tx *sql.Tx, r *db.PendingClaimRequestExport, opts db.ImportOptions) error {
	if opts.DedupeLegacyActivePendingClaims && r.RejectedAt == nil && r.ResolvedAt == nil {
		skip, err := skipLegacyDuplicateActivePendingClaim(ctx, tx, r)
		if err != nil {
			return wrapImportErr(db.ImportKindPendingClaimRequest, err)
		}
		if skip {
			return nil
		}
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO pending_claim_requests(
		   id, request_uid, project_id, issue_id, issue_uid, holder,
		   holder_instance_uid, client_kind, claim_kind, ttl_seconds, purpose, requested_at, last_attempt_at,
		   last_error, rejected_at, resolved_at
		 )
		 VALUES(
		   ?, ?, ?, ?,
		   COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?)),
		   ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 )`,
		r.ID, r.RequestUID, r.ProjectID, r.IssueID,
		r.IssueUID, r.IssueID,
		r.Holder, r.HolderInstanceUID, r.ClientKind, r.ClaimKind, r.TTLSeconds,
		r.Purpose, r.RequestedAt, r.LastAttemptAt, r.LastError, r.RejectedAt,
		r.ResolvedAt)
	return wrapImportErr(db.ImportKindPendingClaimRequest, err)
}

// skipLegacyDuplicateActivePendingClaim returns true if a pre-v12 source
// carries a pending_claim_requests row whose (issue_uid, holder_instance_uid,
// holder, client_kind, still-active) tuple already exists in the target. Pre-
// v12 schemas lacked the uniqueness constraint and could carry duplicates that
// would trip the current schema's enforcement on insert; current-version
// streams skip this dedupe entirely.
func skipLegacyDuplicateActivePendingClaim(ctx context.Context, tx *sql.Tx, rec *db.PendingClaimRequestExport) (bool, error) {
	var existingID int64
	err := tx.QueryRowContext(ctx, `
		SELECT id
		  FROM pending_claim_requests
		 WHERE issue_uid = COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?))
		   AND holder_instance_uid = ?
		   AND holder = ?
		   AND client_kind = ?
		   AND rejected_at IS NULL
		   AND resolved_at IS NULL
		 ORDER BY requested_at ASC, id ASC
		 LIMIT 1`,
		rec.IssueUID, rec.IssueID, rec.HolderInstanceUID, rec.Holder, rec.ClientKind).Scan(&existingID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func importEvent(ctx context.Context, tx *sql.Tx, e *db.EventExport) error {
	if err := fillEventIssueUIDs(ctx, tx, e); err != nil {
		return err // raw: preserves the "corrupt_event_fk: …" prefix asserted by import_test.go
	}
	projectName, err := importedProjectName(ctx, tx, e.ProjectID, e.ProjectName)
	if err != nil {
		return err
	}
	// Validate v13 (current) replay fields; jsonl is responsible for filling
	// pre-v13 sources before the record reaches ImportReplay.
	if e.HLCPhysicalMS <= 0 {
		return fmt.Errorf("event %d missing hlc_physical_ms", e.ID)
	}
	if e.HLCCounter < 0 {
		return fmt.Errorf("event %d has negative hlc_counter", e.ID)
	}
	if !validContentHash(e.ContentHash) {
		return fmt.Errorf("event %d invalid content_hash %q", e.ID, e.ContentHash)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(id, uid, origin_instance_uid, project_id, project_name, issue_id, issue_uid, related_issue_id, related_issue_uid,
		                    type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at)
		 VALUES(
		   ?, ?, ?, ?, ?, ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   ?, ?, ?, ?, ?, ?, ?
		)`,
		e.ID, e.UID, e.OriginInstanceUID,
		e.ProjectID, projectName, e.IssueID,
		stringPtrValue(e.IssueUID), e.IssueID,
		e.RelatedIssueID,
		stringPtrValue(e.RelatedIssueUID), e.RelatedIssueID,
		e.Type, e.Actor, string(e.Payload),
		e.HLCPhysicalMS, e.HLCCounter, e.ContentHash, e.CreatedAt)
	return wrapImportErr(db.ImportKindEvent, err)
}

func importPurgeLog(ctx context.Context, tx *sql.Tx, pl *db.PurgeLogExport) error {
	projectName, err := importedProjectName(ctx, tx, pl.ProjectID, pl.ProjectName)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO purge_log(id, uid, origin_instance_uid, project_id, purged_issue_id, issue_uid, project_uid, project_name, short_id, issue_title,
		                       issue_author, comment_count, link_count, label_count, event_count,
		                       events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
		                       actor, reason, purged_at)
		 VALUES(
		   ?, ?, ?, ?, ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   COALESCE(?, (SELECT uid FROM projects WHERE id = ?)),
		   ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 )`,
		pl.ID, pl.UID, pl.OriginInstanceUID,
		pl.ProjectID, pl.PurgedIssueID,
		stringPtrValue(pl.IssueUID), pl.PurgedIssueID,
		stringPtrValue(pl.ProjectUID), pl.ProjectID,
		projectName, stringPtrValue(pl.ShortID),
		pl.IssueTitle, pl.IssueAuthor, pl.CommentCount, pl.LinkCount, pl.LabelCount,
		pl.EventCount, pl.EventsDeletedMinID, pl.EventsDeletedMaxID, pl.PurgeResetAfterEventID,
		pl.Actor, pl.Reason, pl.PurgedAt)
	return wrapImportErr(db.ImportKindPurgeLog, err)
}

// removeAutoSystemProject deletes the system project row that db.Open inserts on
// every fresh DB so an imported source's system project can take its place
// without colliding on (uid, name). If no row was imported, ensureSystemProject
// re-creates it after the loop.
func removeAutoSystemProject(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM projects WHERE uid = ? AND name = ?`,
		db.SystemProjectUID, db.SystemProjectName)
	if err != nil {
		return fmt.Errorf("remove auto system project before import: %w", err)
	}
	return nil
}

// ensureSystemProject inserts the system project if no project envelope brought
// it in. Idempotent: if the imported source already shipped a system project
// row, this is a no-op.
func ensureSystemProject(ctx context.Context, tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE uid = ? AND name = ?`,
		db.SystemProjectUID, db.SystemProjectName).Scan(&exists); err != nil {
		return fmt.Errorf("check system project after import: %w", err)
	}
	if exists > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO projects(uid, name)
		VALUES(?, ?)
	`, db.SystemProjectUID, db.SystemProjectName)
	if err != nil {
		return fmt.Errorf("ensure system project after import: %w", err)
	}
	return nil
}

// tokenCreatedEventPayload and tokenRevokedEventPayload mirror the daemon-side
// payload shapes so the import-time projector can validate them by structure
// rather than going through json.RawMessage.
type tokenCreatedEventPayload struct {
	TokenID     int64   `json:"token_id"`
	TokenHash   string  `json:"token_hash"`
	TargetActor string  `json:"target_actor"`
	Name        *string `json:"name,omitempty"`
}

type tokenRevokedEventPayload struct {
	TokenID int64 `json:"token_id"`
}

func validateReplayTokenHash(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("token_hash must be 64 hex characters")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("token_hash must be 64 hex characters: %w", err)
	}
	return nil
}

// replayAPITokenProjection rebuilds the api_tokens projection from the
// imported events. Token state is the event log, not a separate snapshot, so
// the import has to re-derive it after every record has landed.
func replayAPITokenProjection(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens`); err != nil {
		return fmt.Errorf("clear api_tokens projection: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.type, e.payload, CAST(e.created_at AS TEXT),
		       e.project_name, p.name, p.uid
		  FROM events e
		  JOIN projects p ON p.id = e.project_id
		 WHERE e.type IN ('token.created', 'token.revoked')
		 ORDER BY e.id ASC`)
	if err != nil {
		return fmt.Errorf("read token events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var typ, payload, createdAt, eventProjectName, projectName, projectUID string
		if err := rows.Scan(&id, &typ, &payload, &createdAt, &eventProjectName, &projectName, &projectUID); err != nil {
			return fmt.Errorf("scan token event: %w", err)
		}
		if projectUID != db.SystemProjectUID || projectName != db.SystemProjectName ||
			eventProjectName != db.SystemProjectName {
			return fmt.Errorf("%s event %d must belong to system project %s",
				typ, id, db.SystemProjectName)
		}
		switch typ {
		case "token.created":
			var rec tokenCreatedEventPayload
			if err := json.Unmarshal([]byte(payload), &rec); err != nil {
				return fmt.Errorf("decode token.created payload: %w", err)
			}
			if rec.TokenID == 0 || rec.TokenHash == "" || rec.TargetActor == "" {
				return fmt.Errorf("decode token.created payload: missing required field")
			}
			if err := validateReplayTokenHash(rec.TokenHash); err != nil {
				return fmt.Errorf("decode token.created payload: %w", err)
			}
			if err := db.ValidateTokenActor(rec.TargetActor); err != nil {
				return fmt.Errorf("decode token.created payload: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO api_tokens(id, token_hash, actor, name, created_at)
				 VALUES(?, ?, ?, ?, ?)`,
				rec.TokenID, rec.TokenHash, rec.TargetActor, rec.Name, createdAt); err != nil {
				return fmt.Errorf("replay token.created %d: %w", rec.TokenID, err)
			}
		case "token.revoked":
			var rec tokenRevokedEventPayload
			if err := json.Unmarshal([]byte(payload), &rec); err != nil {
				return fmt.Errorf("decode token.revoked payload: %w", err)
			}
			if rec.TokenID == 0 {
				return fmt.Errorf("decode token.revoked payload: missing token_id")
			}
			res, err := tx.ExecContext(ctx,
				`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`,
				createdAt, rec.TokenID)
			if err != nil {
				return fmt.Errorf("replay token.revoked %d: %w", rec.TokenID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("replay token.revoked rows affected: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("replay token.revoked %d: token not found", rec.TokenID)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read token event rows: %w", err)
	}
	return nil
}

func uniqueProjectName(ctx context.Context, tx *sql.Tx, projectID int64, name string) (string, bool, error) {
	original := name
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("project-%d", projectID)
		original = name
	}
	if name == db.SystemProjectName {
		name = db.SystemProjectName + "-2"
	}
	for suffix := 1; ; suffix++ {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE name = ?`, name).Scan(&exists); err != nil {
			return "", false, fmt.Errorf("check project name collision: %w", err)
		}
		if exists == 0 {
			return name, name != original, nil
		}
		name = fmt.Sprintf("%s-%d", original, suffix+1)
	}
}

func fillEventIssueUIDs(ctx context.Context, tx *sql.Tx, rec *db.EventExport) error {
	if rec.IssueID != nil && rec.IssueUID == nil {
		issueUID, err := lookupIssueUID(ctx, tx, *rec.IssueID)
		if err != nil {
			return fmt.Errorf("corrupt_event_fk: event %d issue_id %d: %w", rec.ID, *rec.IssueID, err)
		}
		rec.IssueUID = &issueUID
	}
	if rec.RelatedIssueID != nil && rec.RelatedIssueUID == nil {
		issueUID, err := lookupIssueUID(ctx, tx, *rec.RelatedIssueID)
		if err != nil {
			return fmt.Errorf("corrupt_event_fk: event %d related_issue_id %d: %w", rec.ID, *rec.RelatedIssueID, err)
		}
		rec.RelatedIssueUID = &issueUID
	}
	return nil
}

func lookupIssueUID(ctx context.Context, tx *sql.Tx, issueID int64) (string, error) {
	var issueUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM issues WHERE id = ?`, issueID).Scan(&issueUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", db.ErrNotFound
		}
		return "", err
	}
	return issueUID, nil
}

func importedProjectName(ctx context.Context, tx *sql.Tx, projectID int64, projectName string) (string, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, projectID).Scan(&name)
	if err == nil {
		return name, nil
	}
	if projectName != "" {
		return projectName, nil
	}
	return "", fmt.Errorf("project %d not imported before project snapshot: %w", projectID, err)
}

func validContentHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func wrapImportErr(kind string, err error) error {
	if err != nil {
		return fmt.Errorf("import %s: %w", kind, err)
	}
	return nil
}

func recordImportSchemaVersion(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.Itoa(db.CurrentSchemaVersion()))
	if err != nil {
		return fmt.Errorf("record import schema version: %w", err)
	}
	return nil
}

func upsertSequence(ctx context.Context, tx *sql.Tx, name string, seq int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = ?`, seq, name)
	if err != nil {
		return fmt.Errorf("update sqlite_sequence %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite_sequence rows affected: %w", err)
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sqlite_sequence(name, seq) VALUES(?, ?)`, name, seq); err != nil {
			return fmt.Errorf("insert sqlite_sequence %s: %w", name, err)
		}
	}
	return nil
}

func reconcileSequences(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"projects", "project_aliases", "issues", "comments", "links", "import_mappings", "events", "purge_log", "api_tokens", "federation_enrollments"} {
		var maxID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id), 0) FROM `+table).Scan(&maxID); err != nil {
			return fmt.Errorf("max id for %s: %w", table, err)
		}
		var stored sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(seq) FROM sqlite_sequence WHERE name = ?`, table).Scan(&stored); err != nil {
			return fmt.Errorf("read sqlite_sequence %s: %w", table, err)
		}
		seq := maxID
		if stored.Valid && stored.Int64 > seq {
			seq = stored.Int64
		}
		if seq > 0 {
			if err := upsertSequence(ctx, tx, table, seq); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBeforeCommit(ctx context.Context, tx *sql.Tx) error {
	if err := checkForeignKeyViolations(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return fmt.Errorf("integrity_check scan: %w", err)
		}
		if !strings.EqualFold(msg, "ok") {
			return fmt.Errorf("integrity_check: %s", msg)
		}
	}
	return rows.Err()
}

// checkForeignKeyViolations runs PRAGMA foreign_key_check, scans every
// returned row, resolves each violated FK to its column name, and returns a
// single error grouping per-row detail when at least one violation exists.
// Output is capped at 20 rows per child table to bound log size on widely-
// corrupted DBs.
func checkForeignKeyViolations(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	type viol struct {
		Table       string
		RowID       sql.NullInt64
		ParentTable string
		FKID        int
	}
	var all []viol
	for rows.Next() {
		var v viol
		if err := rows.Scan(&v.Table, &v.RowID, &v.ParentTable, &v.FKID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("foreign_key_check scan: %w", err)
		}
		all = append(all, v)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("foreign_key_check rows: %w", err)
	}
	// Close the foreign_key_check rows before the resolver loop: the resolver
	// issues PRAGMA queries on the same *sql.Tx, and SQLite requires the prior
	// result set closed before another statement runs on the connection.
	_ = rows.Close()
	if len(all) == 0 {
		return nil
	}
	resolver := NewFKColumnResolver(tx)
	var sb strings.Builder
	fmt.Fprintf(&sb, "foreign_key_check: %d violations:", len(all))
	truncated := false
	perTable := map[string]int{}
	for _, v := range all {
		if perTable[v.Table] >= 20 {
			truncated = true
			continue
		}
		perTable[v.Table]++
		col, resolveErr := resolver.Resolve(ctx, v.Table, v.FKID)
		if col == "" {
			col = "?"
		}
		rowidStr := "?"
		if v.RowID.Valid {
			rowidStr = fmt.Sprintf("%d", v.RowID.Int64)
		}
		fmt.Fprintf(&sb, "\n  %s rowid=%s parent=%s column=%s", v.Table, rowidStr, v.ParentTable, col)
		if resolveErr != nil {
			fmt.Fprintf(&sb, " (column resolver: %v)", resolveErr)
		}
	}
	if truncated {
		sb.WriteString("\n  (output capped at 20 rows per table)")
	}
	return errors.New(sb.String())
}
