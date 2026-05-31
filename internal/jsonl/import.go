package jsonl

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// ImportOptions controls optional import behaviors.
type ImportOptions struct {
	// NewInstance preserves the target's meta.instance_uid (the value db.Open
	// wrote on first open) instead of overwriting it with the source's. The
	// imported events.origin_instance_uid and purge_log.origin_instance_uid
	// columns are NOT rewritten — they preserve the original origins so a
	// future federation loop-detector can tell which events came from the
	// cloned-from instance versus the new local one.
	NewInstance bool
}

// Import reads JSONL records from r and inserts them into target.
func Import(ctx context.Context, r io.Reader, target *db.DB) error {
	return ImportWithOptions(ctx, r, target, ImportOptions{})
}

// ImportWithOptions is like Import but applies opts to control behavior.
func ImportWithOptions(ctx context.Context, r io.Reader, target *db.DB, opts ImportOptions) error {
	envs, err := NewDecoder(r).ReadAll(ctx)
	if err != nil {
		return err
	}
	exportVersion, err := validateExportVersion(envs)
	if err != nil {
		return err
	}
	// Pre-v8 envelopes lack short_id; derive them in ULID-ascending order
	// per project before any inserts run so the per-envelope loop only ever
	// sees current-version-shaped issue records. Project-name validation
	// (no '#') happens here too so a bad fixture fails before any mutation.
	if exportVersion < 8 {
		if err := applyCutoverV7toV8(envs); err != nil {
			return err
		}
	}
	tx, err := target.BeginTx(ctx, nil)
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

	// Capture the target's meta.instance_uid (set by db.Open) before the
	// envelope loop has a chance to overwrite it. This value is the LOCAL
	// origin used by v2→v3 fill rules even when the source's later upserts
	// replace meta.instance_uid in the same transaction (default-mode v3
	// import).
	localInstanceUID, err := readMetaInstanceUID(ctx, tx)
	if err != nil {
		return err
	}

	for _, env := range envs {
		if err := importEnvelope(ctx, tx, env, exportVersion, localInstanceUID, opts); err != nil {
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
	// Default mode may have overwritten meta.instance_uid with the source's
	// value; the target's cached InstanceUID() must follow suit so subsequent
	// inserts on this handle stamp the right origin. (--new-instance leaves
	// the row at db.Open's value, so the refresh is a no-op there.)
	if err := target.RefreshInstanceUID(ctx); err != nil {
		return err
	}
	return nil
}

func readMetaInstanceUID(ctx context.Context, tx *sql.Tx) (string, error) {
	var v string
	err := tx.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if err != nil {
		return "", fmt.Errorf("read target instance_uid: %w", err)
	}
	return v, nil
}

func removeAutoSystemProject(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM projects WHERE uid = ? AND name = ?`,
		db.SystemProjectUID, db.SystemProjectName)
	if err != nil {
		return fmt.Errorf("remove auto system project before import: %w", err)
	}
	return nil
}

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

func validateReplayTokenHash(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("token_hash must be 64 hex characters")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("token_hash must be 64 hex characters: %w", err)
	}
	return nil
}

type tokenCreatedEventPayload struct {
	TokenID     int64   `json:"token_id"`
	TokenHash   string  `json:"token_hash"`
	TargetActor string  `json:"target_actor"`
	Name        *string `json:"name,omitempty"`
}

type tokenRevokedEventPayload struct {
	TokenID int64 `json:"token_id"`
}

func validateExportVersion(envs []Envelope) (int, error) {
	var rec metaRecord
	if err := decodeData(envs[0], &rec); err != nil {
		return 0, err
	}
	version, err := strconv.Atoi(rec.Value)
	if err != nil {
		return 0, fmt.Errorf("invalid export_version %q: %w", rec.Value, err)
	}
	if version > db.CurrentSchemaVersion() {
		return 0, fmt.Errorf("unsupported export_version %d for current schema version %d", version, db.CurrentSchemaVersion())
	}
	if version < 1 {
		return 0, fmt.Errorf("invalid export_version %d", version)
	}
	return version, nil
}

func importEnvelope(ctx context.Context, tx *sql.Tx, env Envelope, exportVersion int, localInstanceUID string, opts ImportOptions) error {
	switch env.Kind {
	case KindMeta:
		var rec metaRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if rec.Key == "export_version" {
			return nil
		}
		if rec.Key == "schema_version" && exportVersion < db.CurrentSchemaVersion() {
			return nil
		}
		// --new-instance: skip the source's meta.instance_uid record so the
		// target keeps the value db.Open wrote. The imported events and
		// purge_log rows still carry the source's origin_instance_uid (per
		// §5.2): they were authored elsewhere and the new clone needs to know
		// that for future federation loop detection.
		if rec.Key == "instance_uid" && opts.NewInstance {
			return nil
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			rec.Key, rec.Value)
		return wrapImportErr(env.Kind, err)
	case KindProject:
		var rec projectRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeProjectTimes(&rec); err != nil {
			return err
		}
		if err := fillProjectUID(&rec, exportVersion); err != nil {
			return err
		}
		if isSystemProjectRecord(rec) {
			if len(rec.Metadata) == 0 {
				rec.Metadata = json.RawMessage(`{}`)
			}
			if rec.Revision == 0 {
				rec.Revision = 1
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO projects(id, uid, name, created_at, deleted_at, metadata, revision)
				 VALUES(?, ?, ?, ?, ?, ?, ?)`,
				rec.ID, rec.UID, rec.Name, rec.CreatedAt, rec.DeletedAt,
				string(rec.Metadata), rec.Revision)
			return wrapImportErr(env.Kind, err)
		}
		rec.OriginalName = rec.Name
		name, renamed, err := uniqueProjectName(ctx, tx, rec.ID, rec.Name)
		if err != nil {
			return err
		}
		rec.Name = name
		if renamed {
			fmt.Fprintf(os.Stderr, "note: project #%d renamed from %q to %q during cutover\n", rec.ID, rec.OriginalName, rec.Name)
		}
		if len(rec.Metadata) == 0 {
			rec.Metadata = json.RawMessage(`{}`)
		}
		if rec.Revision == 0 {
			rec.Revision = 1
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO projects(id, uid, name, created_at, deleted_at, metadata, revision)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.UID, rec.Name, rec.CreatedAt, rec.DeletedAt,
			string(rec.Metadata), rec.Revision)
		return wrapImportErr(env.Kind, err)
	case KindProjectAlias:
		var rec projectAliasRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeProjectAliasTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO project_aliases(id, project_id, alias_identity, alias_kind, root_path, created_at, last_seen_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.ProjectID, rec.AliasIdentity, rec.AliasKind, rec.RootPath, rec.CreatedAt, rec.LastSeenAt)
		return wrapImportErr(env.Kind, err)
	case KindRecurrence:
		var rec recurrenceRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeRecurrenceTimes(&rec); err != nil {
			return err
		}
		if len(rec.TemplateLabels) == 0 {
			rec.TemplateLabels = json.RawMessage(`[]`)
		}
		if len(rec.TemplateMetadata) == 0 {
			rec.TemplateMetadata = json.RawMessage(`{}`)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO recurrences
			   (id, uid, project_id, rrule, dtstart, timezone,
			    template_title, template_body, template_owner, template_priority,
			    template_labels, template_metadata,
			    next_occurrence_key, last_materialized_uid,
			    author, revision, created_at, updated_at, deleted_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.UID, rec.ProjectID, rec.RRule, rec.DTStart, rec.Timezone,
			rec.TemplateTitle, rec.TemplateBody, rec.TemplateOwner, rec.TemplatePriority,
			string(rec.TemplateLabels), string(rec.TemplateMetadata),
			rec.NextOccurrenceKey, rec.LastMaterializedUID,
			rec.Author, rec.Revision, rec.CreatedAt, rec.UpdatedAt, rec.DeletedAt)
		return wrapImportErr(env.Kind, err)
	case KindIssue:
		var rec issueRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := fillIssueUID(&rec, exportVersion); err != nil {
			return err
		}
		if err := normalizeIssueTimes(&rec); err != nil {
			return err
		}
		// Current-version envelopes carry short_id verbatim; older
		// envelopes (no short_id) fail this branch and require the
		// v7→v8 cutover (Task 9) to derive a short_id from the UID.
		if rec.ShortID == "" {
			return fmt.Errorf("import issue %d: missing short_id (older envelopes must go through cutover)", rec.ID)
		}
		if len(rec.Metadata) == 0 {
			rec.Metadata = json.RawMessage(`{}`)
		}
		if rec.Revision == 0 {
			rec.Revision = 1
		}
		// Co-field validation: occurrence_key requires recurrence linkage.
		if rec.OccurrenceKey != nil && rec.RecurrenceUID == nil && rec.RecurrenceID == nil {
			return fmt.Errorf("import issue %d (uid=%s): occurrence_key set without recurrence_uid",
				rec.ID, rec.UID)
		}
		if rec.RecurrenceUID != nil {
			var resolvedID int64
			if qErr := tx.QueryRowContext(ctx,
				`SELECT id FROM recurrences WHERE uid = ?`, *rec.RecurrenceUID,
			).Scan(&resolvedID); qErr != nil {
				return fmt.Errorf("import issue %d: recurrence_uid %q not found: %w",
					rec.ID, *rec.RecurrenceUID, qErr)
			}
			if rec.RecurrenceID != nil && *rec.RecurrenceID != resolvedID {
				return fmt.Errorf("import issue %d: recurrence_uid %q resolves to id %d, but record carries recurrence_id %d",
					rec.ID, *rec.RecurrenceUID, resolvedID, *rec.RecurrenceID)
			}
			rec.RecurrenceID = &resolvedID
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO issues(id, uid, project_id, short_id, title, body, status, closed_reason, owner, priority, author,
			                    created_at, updated_at, closed_at, deleted_at, metadata, revision,
			                    recurrence_id, occurrence_key)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.UID, rec.ProjectID, rec.ShortID, rec.Title, rec.Body, rec.Status, rec.ClosedReason,
			rec.Owner, rec.Priority, rec.Author, rec.CreatedAt, rec.UpdatedAt, rec.ClosedAt, rec.DeletedAt,
			string(rec.Metadata), rec.Revision,
			rec.RecurrenceID, rec.OccurrenceKey)
		return wrapImportErr(env.Kind, err)
	case KindComment:
		var rec commentRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := fillCommentUID(&rec); err != nil {
			return err
		}
		if err := normalizeCommentTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO comments(id, uid, issue_id, author, body, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.UID, rec.IssueID, rec.Author, rec.Body, rec.CreatedAt)
		return wrapImportErr(env.Kind, err)
	case KindIssueLabel:
		var rec issueLabelRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeIssueLabelTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO issue_labels(issue_id, label, author, created_at) VALUES(?, ?, ?, ?)`,
			rec.IssueID, rec.Label, rec.Author, rec.CreatedAt)
		return wrapImportErr(env.Kind, err)
	case KindLink:
		var rec linkRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeLinkTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO links(id, project_id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author, created_at)
			 VALUES(
			   ?, ?, ?,
			   COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?)),
			   ?,
			   COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?)),
			   ?, ?, ?
			 )`,
			rec.ID, rec.ProjectID, rec.FromIssueID, rec.FromIssueUID, rec.FromIssueID,
			rec.ToIssueID, rec.ToIssueUID, rec.ToIssueID, rec.Type, rec.Author, rec.CreatedAt)
		return wrapImportErr(env.Kind, err)
	case KindImportMapping:
		var rec importMappingRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeImportMappingTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO import_mappings(id, source, external_id, object_type, project_id, issue_id, comment_id, link_id, label, source_updated_at, imported_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.Source, rec.ExternalID, rec.ObjectType, rec.ProjectID, rec.IssueID, rec.CommentID,
			rec.LinkID, rec.Label, rec.SourceUpdatedAt, rec.ImportedAt)
		return wrapImportErr(env.Kind, err)
	case KindFederationBinding:
		var rec federationBindingRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeFederationBindingTimes(&rec); err != nil {
			return err
		}
		enabled := 0
		if rec.Enabled {
			enabled = 1
		}
		pushEnabled := 0
		if rec.PushEnabled {
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
			rec.ProjectID, rec.Role, rec.HubURL, rec.HubProjectID, rec.HubProjectUID,
			rec.ReplayHorizonEventID, rec.PullCursorEventID, pushEnabled,
			rec.PushCursorEventID, enabled,
			rec.CreatedAt, rec.UpdatedAt, rec.LastSyncAt)
		return wrapImportErr(env.Kind, err)
	case KindFederationSyncStatus:
		var rec federationSyncStatusRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeFederationSyncStatusTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO federation_sync_status(
			   project_id, last_pull_started_at, last_pull_success_at,
			   last_push_started_at, last_push_success_at,
			   last_error_at, last_error, last_reset_at
			 )
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ProjectID, rec.LastPullStartedAt, rec.LastPullSuccessAt,
			rec.LastPushStartedAt, rec.LastPushSuccessAt,
			rec.LastErrorAt, rec.LastError, rec.LastResetAt)
		return wrapImportErr(env.Kind, err)
	case KindFederationQuarantine:
		var rec federationQuarantineRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeFederationQuarantineTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO federation_quarantine(
			   id, project_id, direction, first_event_id, last_event_id,
			   event_uids, error, created_at, skipped_at, skipped_by, skip_reason
			 )
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.ProjectID, rec.Direction, rec.FirstEventID, rec.LastEventID,
			string(rec.EventUIDs), rec.Error, rec.CreatedAt, rec.SkippedAt, rec.SkippedBy, rec.SkipReason)
		return wrapImportErr(env.Kind, err)
	case KindFederationEnrollment:
		var rec federationEnrollmentRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeFederationEnrollmentTimes(&rec); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO federation_enrollments(
			   id, token_hash, spoke_instance_uid, project_id, capabilities,
			   created_at, updated_at, revoked_at
			 )
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.TokenHash, rec.SpokeInstanceUID, rec.ProjectID, rec.Capabilities,
			rec.CreatedAt, rec.UpdatedAt, rec.RevokedAt)
		return wrapImportErr(env.Kind, err)
	case KindIssueClaim:
		var rec issueClaimRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeIssueClaimTimes(&rec); err != nil {
			return err
		}
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
			rec.ID, rec.ClaimUID, rec.ProjectID, rec.IssueID,
			rec.IssueUID, rec.IssueID,
			rec.Holder, rec.HolderInstanceUID, rec.ClientKind, rec.Purpose,
			rec.ClaimKind, rec.AcquiredAt, rec.ExpiresAt, rec.ReleasedAt,
			rec.ReleaseReason, rec.Revision, rec.UpdatedAt)
		return wrapImportErr(env.Kind, err)
	case KindPendingClaimRequest:
		var rec pendingClaimRequestRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizePendingClaimRequestTimes(&rec); err != nil {
			return err
		}
		skip, err := skipLegacyDuplicateActivePendingClaim(ctx, tx, rec, exportVersion)
		if err != nil {
			return wrapImportErr(env.Kind, err)
		}
		if skip {
			return nil
		}
		_, err = tx.ExecContext(ctx,
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
			rec.ID, rec.RequestUID, rec.ProjectID, rec.IssueID,
			rec.IssueUID, rec.IssueID,
			rec.Holder, rec.HolderInstanceUID, rec.ClientKind, rec.ClaimKind, rec.TTLSeconds,
			rec.Purpose, rec.RequestedAt, rec.LastAttemptAt, rec.LastError, rec.RejectedAt,
			rec.ResolvedAt)
		return wrapImportErr(env.Kind, err)
	case KindEvent:
		var rec eventRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := normalizeEventTimes(&rec); err != nil {
			return err
		}
		if err := fillEventUIDs(ctx, tx, &rec); err != nil {
			return err
		}
		if err := fillEventV3Identity(&rec, exportVersion, localInstanceUID); err != nil {
			return err
		}
		projectName, err := importedProjectName(ctx, tx, rec.ProjectID, rec.ProjectName, rec.LegacyProjectName)
		if err != nil {
			return err
		}
		rec.ProjectName = projectName
		if err := fillEventV11ReplayFields(ctx, tx, &rec, exportVersion); err != nil {
			return err
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
			rec.ID, rec.UID, rec.OriginInstanceUID,
			rec.ProjectID, projectName, rec.IssueID,
			stringPtrValue(rec.IssueUID), rec.IssueID,
			rec.RelatedIssueID,
			stringPtrValue(rec.RelatedIssueUID), rec.RelatedIssueID,
			rec.Type, rec.Actor, string(rec.Payload),
			rec.HLCPhysicalMS, rec.HLCCounter, rec.ContentHash, rec.CreatedAt)
		return wrapImportErr(env.Kind, err)
	case KindPurgeLog:
		var rec purgeLogRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		if err := fillPurgeLogV3Identity(&rec, exportVersion, localInstanceUID); err != nil {
			return err
		}
		if err := normalizePurgeLogTimes(&rec); err != nil {
			return err
		}
		projectName, err := importedProjectName(ctx, tx, rec.ProjectID, rec.ProjectName, rec.LegacyProjectName)
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
			rec.ID, rec.UID, rec.OriginInstanceUID,
			rec.ProjectID, rec.PurgedIssueID,
			stringPtrValue(rec.IssueUID), rec.PurgedIssueID,
			stringPtrValue(rec.ProjectUID), rec.ProjectID,
			projectName, stringPtrValue(rec.ShortID),
			rec.IssueTitle, rec.IssueAuthor, rec.CommentCount, rec.LinkCount, rec.LabelCount,
			rec.EventCount, rec.EventsDeletedMinID, rec.EventsDeletedMaxID, rec.PurgeResetAfterEventID,
			rec.Actor, rec.Reason, rec.PurgedAt)
		return wrapImportErr(env.Kind, err)
	case KindSQLiteSequence:
		var rec sqliteSequenceRecord
		if err := decodeData(env, &rec); err != nil {
			return err
		}
		return upsertSequence(ctx, tx, rec.Name, rec.Seq)
	default:
		return fmt.Errorf("import %s: unsupported kind", env.Kind)
	}
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

func decodeData(env Envelope, dst any) error {
	if err := json.Unmarshal(env.Data, dst); err != nil {
		return fmt.Errorf("decode %s data: %w", env.Kind, err)
	}
	return nil
}

func wrapImportErr(kind Kind, err error) error {
	if err != nil {
		return fmt.Errorf("import %s: %w", kind, err)
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

func isSystemProjectRecord(rec projectRecord) bool {
	return rec.UID == db.SystemProjectUID && rec.Name == db.SystemProjectName
}

func fillProjectUID(rec *projectRecord, exportVersion int) error {
	if exportVersion >= 2 || rec.UID != "" {
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill project uid: %w", err)
	}
	uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("project:%d:%s", rec.ID, rec.Identity)), t)
	if err != nil {
		return fmt.Errorf("fill project uid: %w", err)
	}
	rec.UID = uid
	return nil
}

func fillIssueUID(rec *issueRecord, exportVersion int) error {
	if exportVersion >= 2 || rec.UID != "" {
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill issue uid: %w", err)
	}
	uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("issue:%d:%d", rec.ProjectID, rec.Number)), t)
	if err != nil {
		return fmt.Errorf("fill issue uid: %w", err)
	}
	rec.UID = uid
	return nil
}

func fillCommentUID(rec *commentRecord) error {
	if rec.UID != "" {
		if !katauid.Valid(rec.UID) {
			return fmt.Errorf("invalid comment uid %q", rec.UID)
		}
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill comment uid: %w", err)
	}
	uid, err := katauid.FromStableSeed(
		[]byte(fmt.Sprintf("comment:%d:%d:%s:%s:%s", rec.IssueID, rec.ID, rec.Author, rec.Body, rec.CreatedAt)),
		t,
	)
	if err != nil {
		return fmt.Errorf("fill comment uid: %w", err)
	}
	rec.UID = uid
	return nil
}

func fillEventUIDs(ctx context.Context, tx *sql.Tx, rec *eventRecord) error {
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

// fillEventV3Identity backfills events.uid + events.origin_instance_uid for
// pre-v3 sources per spec §5.3. The event UID is deterministic across reruns
// (FromStableSeed of project_id+id+created_at). The origin_instance_uid is the
// destination's local instance UID — intentionally non-deterministic across
// reruns: re-cutover from the same v2 source produces a different LOCAL and
// therefore different origins on every backfilled event. v3 sources carry both
// fields verbatim.
func fillEventV3Identity(rec *eventRecord, exportVersion int, localInstanceUID string) error {
	if exportVersion >= 3 {
		return nil
	}
	if rec.UID == "" {
		t, err := parseExportTime(rec.CreatedAt)
		if err != nil {
			return fmt.Errorf("fill event uid: %w", err)
		}
		uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("event:%d:%d", rec.ProjectID, rec.ID)), t)
		if err != nil {
			return fmt.Errorf("fill event uid: %w", err)
		}
		rec.UID = uid
	}
	if rec.OriginInstanceUID == "" {
		rec.OriginInstanceUID = localInstanceUID
	}
	return nil
}

func fillEventV11ReplayFields(ctx context.Context, tx *sql.Tx, rec *eventRecord, exportVersion int) error {
	if exportVersion >= db.CurrentSchemaVersion() {
		if rec.HLCPhysicalMS <= 0 {
			return fmt.Errorf("event %d missing hlc_physical_ms", rec.ID)
		}
		if rec.HLCCounter < 0 {
			return fmt.Errorf("event %d has negative hlc_counter", rec.ID)
		}
		if !validContentHash(rec.ContentHash) {
			return fmt.Errorf("event %d invalid content_hash %q", rec.ID, rec.ContentHash)
		}
		if err := validateEventContentHash(ctx, tx, rec); err != nil {
			return err
		}
		return nil
	}
	t, err := parseExportTime(rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("fill event replay fields: %w", err)
	}
	if exportVersion < 12 {
		rec.CreatedAt = formatImportTime(t)
	}
	if exportVersion >= 12 {
		if rec.HLCPhysicalMS <= 0 {
			return fmt.Errorf("event %d missing hlc_physical_ms", rec.ID)
		}
		if rec.HLCCounter < 0 {
			return fmt.Errorf("event %d has negative hlc_counter", rec.ID)
		}
	} else {
		if rec.HLCPhysicalMS <= 0 {
			rec.HLCPhysicalMS = t.UTC().UnixMilli()
			rec.HLCCounter = rec.ID
		} else if rec.HLCCounter < 0 {
			return fmt.Errorf("event %d has negative hlc_counter", rec.ID)
		}
	}
	projectUID, err := lookupProjectUID(ctx, tx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("fill event replay fields: %w", err)
	}
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               rec.UID,
		OriginInstanceUID: rec.OriginInstanceUID,
		ProjectUID:        projectUID,
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
		return fmt.Errorf("fill event content hash: %w", err)
	}
	rec.ContentHash = hash
	return nil
}

func validateEventContentHash(ctx context.Context, tx *sql.Tx, rec *eventRecord) error {
	projectUID, err := lookupProjectUID(ctx, tx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("validate event content hash: %w", err)
	}
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               rec.UID,
		OriginInstanceUID: rec.OriginInstanceUID,
		ProjectUID:        projectUID,
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
		return fmt.Errorf("validate event content hash: %w", err)
	}
	if hash != rec.ContentHash {
		return fmt.Errorf("event %d content_hash mismatch: got %s, want %s", rec.ID, rec.ContentHash, hash)
	}
	return nil
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

func lookupProjectUID(ctx context.Context, tx *sql.Tx, projectID int64) (string, error) {
	var uid string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id = ?`, projectID).Scan(&uid); err != nil {
		return "", fmt.Errorf("lookup project uid %d: %w", projectID, err)
	}
	return uid, nil
}

// fillPurgeLogV3Identity backfills purge_log.uid + purge_log.origin_instance_uid
// for pre-v3 sources per spec §5.3. Mirrors fillEventV3Identity.
func fillPurgeLogV3Identity(rec *purgeLogRecord, exportVersion int, localInstanceUID string) error {
	if exportVersion >= 3 {
		return nil
	}
	if rec.UID == "" {
		t, err := parseExportTime(rec.PurgedAt)
		if err != nil {
			return fmt.Errorf("fill purge_log uid: %w", err)
		}
		uid, err := katauid.FromStableSeed([]byte(fmt.Sprintf("purge:%d:%d", rec.ProjectID, rec.ID)), t)
		if err != nil {
			return fmt.Errorf("fill purge_log uid: %w", err)
		}
		rec.UID = uid
	}
	if rec.OriginInstanceUID == "" {
		rec.OriginInstanceUID = localInstanceUID
	}
	return nil
}

func lookupIssueUID(ctx context.Context, tx *sql.Tx, issueID int64) (string, error) {
	var issueUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM issues WHERE id = ?`, issueID).Scan(&issueUID); err != nil {
		if err == sql.ErrNoRows {
			return "", db.ErrNotFound
		}
		return "", err
	}
	return issueUID, nil
}

func parseExportTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse timestamp %q", s)
}

func formatImportTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func normalizeImportTime(field string, value *string) error {
	if value == nil {
		return nil
	}
	t, err := parseExportTime(*value)
	if err != nil {
		return fmt.Errorf("normalize %s: %w", field, err)
	}
	*value = formatImportTime(t)
	return nil
}

func normalizeProjectTimes(rec *projectRecord) error {
	if err := normalizeImportTime("project.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	return normalizeImportTime("project.deleted_at", rec.DeletedAt)
}

func normalizeProjectAliasTimes(rec *projectAliasRecord) error {
	if err := normalizeImportTime("project_alias.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	return normalizeImportTime("project_alias.last_seen_at", &rec.LastSeenAt)
}

func normalizeRecurrenceTimes(rec *recurrenceRecord) error {
	if err := normalizeImportTime("recurrence.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("recurrence.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("recurrence.deleted_at", rec.DeletedAt)
}

func normalizeIssueTimes(rec *issueRecord) error {
	if err := normalizeImportTime("issue.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue.closed_at", rec.ClosedAt); err != nil {
		return err
	}
	return normalizeImportTime("issue.deleted_at", rec.DeletedAt)
}

func normalizeCommentTimes(rec *commentRecord) error {
	return normalizeImportTime("comment.created_at", &rec.CreatedAt)
}

func normalizeIssueLabelTimes(rec *issueLabelRecord) error {
	return normalizeImportTime("issue_label.created_at", &rec.CreatedAt)
}

func normalizeLinkTimes(rec *linkRecord) error {
	return normalizeImportTime("link.created_at", &rec.CreatedAt)
}

func normalizeImportMappingTimes(rec *importMappingRecord) error {
	if err := normalizeImportTime("import_mapping.source_updated_at", rec.SourceUpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("import_mapping.imported_at", &rec.ImportedAt)
}

func normalizeFederationBindingTimes(rec *federationBindingRecord) error {
	if err := normalizeImportTime("federation_binding.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_binding.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("federation_binding.last_sync_at", rec.LastSyncAt)
}

func normalizeFederationSyncStatusTimes(rec *federationSyncStatusRecord) error {
	for field, value := range map[string]*string{
		"federation_sync_status.last_pull_started_at": rec.LastPullStartedAt,
		"federation_sync_status.last_pull_success_at": rec.LastPullSuccessAt,
		"federation_sync_status.last_push_started_at": rec.LastPushStartedAt,
		"federation_sync_status.last_push_success_at": rec.LastPushSuccessAt,
		"federation_sync_status.last_error_at":        rec.LastErrorAt,
		"federation_sync_status.last_reset_at":        rec.LastResetAt,
	} {
		if err := normalizeImportTime(field, value); err != nil {
			return err
		}
	}
	return nil
}

func normalizeFederationQuarantineTimes(rec *federationQuarantineRecord) error {
	if err := normalizeImportTime("federation_quarantine.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	return normalizeImportTime("federation_quarantine.skipped_at", rec.SkippedAt)
}

func normalizeFederationEnrollmentTimes(rec *federationEnrollmentRecord) error {
	if err := normalizeImportTime("federation_enrollment.created_at", &rec.CreatedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("federation_enrollment.updated_at", &rec.UpdatedAt); err != nil {
		return err
	}
	return normalizeImportTime("federation_enrollment.revoked_at", rec.RevokedAt)
}

func normalizeIssueClaimTimes(rec *issueClaimRecord) error {
	if err := normalizeImportTime("issue_claim.acquired_at", &rec.AcquiredAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue_claim.expires_at", rec.ExpiresAt); err != nil {
		return err
	}
	if err := normalizeImportTime("issue_claim.released_at", rec.ReleasedAt); err != nil {
		return err
	}
	return normalizeImportTime("issue_claim.updated_at", &rec.UpdatedAt)
}

func normalizePendingClaimRequestTimes(rec *pendingClaimRequestRecord) error {
	if err := normalizeImportTime("pending_claim_request.requested_at", &rec.RequestedAt); err != nil {
		return err
	}
	if err := normalizeImportTime("pending_claim_request.last_attempt_at", rec.LastAttemptAt); err != nil {
		return err
	}
	if err := normalizeImportTime("pending_claim_request.rejected_at", rec.RejectedAt); err != nil {
		return err
	}
	return normalizeImportTime("pending_claim_request.resolved_at", rec.ResolvedAt)
}

func normalizeEventTimes(rec *eventRecord) error {
	return normalizeImportTime("event.created_at", &rec.CreatedAt)
}

func normalizePurgeLogTimes(rec *purgeLogRecord) error {
	return normalizeImportTime("purge_log.purged_at", &rec.PurgedAt)
}

func stringPtrValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

type projectRecord struct {
	ID           int64  `json:"id"`
	UID          string `json:"uid"`
	Identity     string `json:"identity,omitempty"`
	Name         string `json:"name"`
	OriginalName string `json:"-"`
	CreatedAt    string `json:"created_at"`
	// NextIssueNumber is the legacy per-project counter; carried for
	// v7-and-below envelopes so the decoder can read them. Dropped from
	// the current version's exports (v8+) — see export.go.
	NextIssueNumber int64   `json:"next_issue_number,omitempty"`
	DeletedAt       *string `json:"deleted_at,omitempty"`
	// Metadata and Revision land in v10 envelopes; pre-v10 sources omit
	// the fields and the importer defaults them to '{}'/1 before INSERT.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Revision int64           `json:"revision,omitempty"`
}

type projectAliasRecord struct {
	ID            int64  `json:"id"`
	ProjectID     int64  `json:"project_id"`
	AliasIdentity string `json:"alias_identity"`
	AliasKind     string `json:"alias_kind"`
	RootPath      string `json:"root_path"`
	CreatedAt     string `json:"created_at"`
	LastSeenAt    string `json:"last_seen_at"`
}

type recurrenceRecord struct {
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

func skipLegacyDuplicateActivePendingClaim(
	ctx context.Context,
	tx *sql.Tx,
	rec pendingClaimRequestRecord,
	exportVersion int,
) (bool, error) {
	if exportVersion >= 12 || rec.RejectedAt != nil || rec.ResolvedAt != nil {
		return false, nil
	}
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

type issueRecord struct {
	ID        int64  `json:"id"`
	UID       string `json:"uid"`
	ProjectID int64  `json:"project_id"`
	// ShortID is the v8+ display ID. Older envelopes (v7 and below) omit
	// this field and carry Number instead; the cutover (Task 9) derives
	// short_ids from UIDs for those.
	ShortID string `json:"short_id,omitempty"`
	// Number is the legacy per-project counter; carried for v7-and-below
	// envelopes so the decoder can read them. Dropped from the current
	// version's exports (v8+) — see export.go.
	Number       int64   `json:"number,omitempty"`
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
	// Metadata and Revision land in v10 envelopes; pre-v10 sources omit
	// the fields and the importer defaults them to '{}'/1 before INSERT.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Revision int64           `json:"revision,omitempty"`
	// Recurrence linkage fields land in v10+ envelopes. RecurrenceUID is the
	// portable identifier joined from recurrences.uid at export; RecurrenceID
	// is the source-side row id (echoed for diagnostics). Import resolves
	// RecurrenceUID against the target DB and validates against RecurrenceID
	// when both are present.
	RecurrenceID  *int64  `json:"recurrence_id,omitempty"`
	RecurrenceUID *string `json:"recurrence_uid,omitempty"`
	OccurrenceKey *string `json:"occurrence_key,omitempty"`
}

type commentRecord struct {
	ID        int64  `json:"id"`
	UID       string `json:"uid"`
	IssueID   int64  `json:"issue_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type issueLabelRecord struct {
	IssueID   int64  `json:"issue_id"`
	Label     string `json:"label"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

type linkRecord struct {
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

type importMappingRecord struct {
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

type federationBindingRecord struct {
	ProjectID            int64   `json:"project_id"`
	Role                 string  `json:"role"`
	HubURL               string  `json:"hub_url"`
	HubProjectID         int64   `json:"hub_project_id"`
	HubProjectUID        string  `json:"hub_project_uid"`
	ReplayHorizonEventID int64   `json:"replay_horizon_event_id"`
	PullCursorEventID    int64   `json:"pull_cursor_event_id"`
	PushEnabled          bool    `json:"push_enabled,omitempty"`
	PushCursorEventID    int64   `json:"push_cursor_event_id,omitempty"`
	Enabled              bool    `json:"enabled"`
	CreatedAt            string  `json:"created_at"`
	UpdatedAt            string  `json:"updated_at"`
	LastSyncAt           *string `json:"last_sync_at,omitempty"`
}

type federationSyncStatusRecord struct {
	ProjectID         int64   `json:"project_id"`
	LastPullStartedAt *string `json:"last_pull_started_at,omitempty"`
	LastPullSuccessAt *string `json:"last_pull_success_at,omitempty"`
	LastPushStartedAt *string `json:"last_push_started_at,omitempty"`
	LastPushSuccessAt *string `json:"last_push_success_at,omitempty"`
	LastErrorAt       *string `json:"last_error_at,omitempty"`
	LastError         *string `json:"last_error,omitempty"`
	LastResetAt       *string `json:"last_reset_at,omitempty"`
}

type federationQuarantineRecord struct {
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

type federationEnrollmentRecord struct {
	ID               int64   `json:"id"`
	TokenHash        string  `json:"token_hash"`
	SpokeInstanceUID string  `json:"spoke_instance_uid"`
	ProjectID        *int64  `json:"project_id,omitempty"`
	Capabilities     string  `json:"capabilities"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	RevokedAt        *string `json:"revoked_at,omitempty"`
}

type issueClaimRecord struct {
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

type pendingClaimRequestRecord struct {
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

type eventRecord struct {
	ID                int64   `json:"id"`
	UID               string  `json:"uid"`
	OriginInstanceUID string  `json:"origin_instance_uid"`
	ProjectID         int64   `json:"project_id"`
	ProjectName       string  `json:"project_name"`
	LegacyProjectName string  `json:"project_identity,omitempty"`
	IssueID           *int64  `json:"issue_id"`
	IssueUID          *string `json:"issue_uid"`
	// IssueNumber is the legacy per-project counter snapshot; carried for
	// v7-and-below envelopes so the decoder can read them. Dropped from
	// the current version's exports (v8+) — see export.go.
	IssueNumber     *int64          `json:"issue_number,omitempty"`
	RelatedIssueID  *int64          `json:"related_issue_id"`
	RelatedIssueUID *string         `json:"related_issue_uid"`
	Type            string          `json:"type"`
	Actor           string          `json:"actor"`
	Payload         json.RawMessage `json:"payload"`
	HLCPhysicalMS   int64           `json:"hlc_physical_ms,omitempty"`
	HLCCounter      int64           `json:"hlc_counter,omitempty"`
	ContentHash     string          `json:"content_hash,omitempty"`
	CreatedAt       string          `json:"created_at"`
}

type purgeLogRecord struct {
	ID                int64   `json:"id"`
	UID               string  `json:"uid"`
	OriginInstanceUID string  `json:"origin_instance_uid"`
	ProjectID         int64   `json:"project_id"`
	PurgedIssueID     int64   `json:"purged_issue_id"`
	IssueUID          *string `json:"issue_uid"`
	ProjectUID        *string `json:"project_uid"`
	ProjectName       string  `json:"project_name"`
	LegacyProjectName string  `json:"project_identity,omitempty"`
	// IssueNumber is the legacy per-project counter snapshot; carried for
	// v7-and-below envelopes so the decoder can read them. Dropped from
	// the current version's exports (v8+) — see export.go.
	IssueNumber int64 `json:"issue_number,omitempty"`
	// ShortID is the purged issue's short_id snapshot; populated for v8+
	// envelopes, NULL/absent for v7-and-below entries that pre-date
	// short_ids. NULL tombstones don't gate anything in assignShortIDIn.
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

type sqliteSequenceRecord struct {
	Name string `json:"name"`
	Seq  int64  `json:"seq"`
}

func importedProjectName(ctx context.Context, tx *sql.Tx, projectID int64, projectName, legacyProjectIdentity string) (string, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, projectID).Scan(&name)
	if err == nil {
		return name, nil
	}
	if projectName != "" {
		return projectName, nil
	}
	if legacyProjectIdentity != "" {
		return "", fmt.Errorf("project %d not imported before legacy project snapshot %q: %w", projectID, legacyProjectIdentity, err)
	}
	return "", fmt.Errorf("project %d not imported before project snapshot: %w", projectID, err)
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
// returned row, resolves each violated FK to its column name, and
// returns a single error grouping per-row detail when at least one
// violation exists. Output is capped at 20 rows per child table to
// bound log size on widely-corrupted DBs.
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
	// Close the foreign_key_check rows before the resolver loop:
	// the resolver issues PRAGMA queries on the same *sql.Tx, and
	// SQLite requires the prior result set closed before another
	// statement runs on the connection.
	_ = rows.Close()
	if len(all) == 0 {
		return nil
	}
	resolver := newFKColumnResolver(tx)
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
		col, resolveErr := resolver.resolve(ctx, v.Table, v.FKID)
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
