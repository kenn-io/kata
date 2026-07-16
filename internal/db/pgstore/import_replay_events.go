package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/kata/internal/db"
)

func pgReplayEvent(
	ctx context.Context,
	tx *sql.Tx,
	event *db.EventExport,
	opts db.ImportOptions,
) error {
	if err := pgReplayFillEventIssueUIDs(ctx, tx, event); err != nil {
		return err
	}
	currentProjectName, projectUID, err := pgReplayEventProjectIdentity(ctx, tx, event)
	if err != nil {
		return err
	}
	durableProjectName, err := db.ReplayEventProjectName(
		event, currentProjectName, opts.RecomputeEventContentHash,
	)
	if err != nil {
		return err
	}
	if err := db.PrepareReplayEvent(
		event, projectUID, durableProjectName, opts.RecomputeEventContentHash,
	); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(
id,uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,
related_issue_id,related_issue_uid,type,actor,payload,hlc_physical_ms,
hlc_counter,content_hash,created_at
) OVERRIDING SYSTEM VALUE VALUES(
$1,$2,$3,$4,$5,$6,COALESCE($7,(SELECT uid FROM issues WHERE id=$6)),
$8,COALESCE($9,(SELECT uid FROM issues WHERE id=$8)),$10,$11,$12,$13,$14,$15,$16
)`, event.ID, event.UID, event.OriginInstanceUID, event.ProjectID, durableProjectName,
		event.IssueID, event.IssueUID, event.RelatedIssueID, event.RelatedIssueUID,
		event.Type, event.Actor, string(event.Payload), event.HLCPhysicalMS,
		event.HLCCounter, event.ContentHash, event.CreatedAt)
	return pgReplayError(db.ImportKindEvent, err)
}

func pgReplayFillEventIssueUIDs(ctx context.Context, tx *sql.Tx, event *db.EventExport) error {
	if event.IssueID != nil && event.IssueUID == nil {
		issueUID, err := pgReplayIssueUID(ctx, tx, *event.IssueID)
		if err != nil {
			return fmt.Errorf("corrupt_event_fk: event %d issue_id %d: %w",
				event.ID, *event.IssueID, err)
		}
		event.IssueUID = &issueUID
	}
	if event.RelatedIssueID != nil && event.RelatedIssueUID == nil {
		issueUID, err := pgReplayIssueUID(ctx, tx, *event.RelatedIssueID)
		if err != nil {
			return fmt.Errorf("corrupt_event_fk: event %d related_issue_id %d: %w",
				event.ID, *event.RelatedIssueID, err)
		}
		event.RelatedIssueUID = &issueUID
	}
	return nil
}

func pgReplayIssueUID(ctx context.Context, tx *sql.Tx, issueID int64) (string, error) {
	var issueUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM issues WHERE id=$1`, issueID).Scan(&issueUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", db.ErrNotFound
		}
		return "", mapSQLError(err, nil)
	}
	return issueUID, nil
}

func pgReplayEventProjectIdentity(
	ctx context.Context,
	tx *sql.Tx,
	event *db.EventExport,
) (name string, uid string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT name,uid FROM projects WHERE id=$1`, event.ProjectID).
		Scan(&name, &uid)
	if err == nil {
		return name, uid, nil
	}
	if event.ProjectName != "" && event.ProjectUID != "" {
		return event.ProjectName, event.ProjectUID, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("project %d not imported before event %d: %w",
			event.ProjectID, event.ID, db.ErrNotFound)
	}
	return "", "", mapSQLError(err, nil)
}

func pgReplayPurgeLog(ctx context.Context, tx *sql.Tx, purge *db.PurgeLogExport) error {
	projectName, err := pgReplayProjectName(ctx, tx, purge.ProjectID, purge.ProjectName)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO purge_log(
id,uid,origin_instance_uid,project_id,purged_issue_id,issue_uid,project_uid,
project_name,short_id,issue_title,issue_author,comment_count,link_count,label_count,
event_count,events_deleted_min_id,events_deleted_max_id,purge_reset_after_event_id,
actor,reason,purged_at
) OVERRIDING SYSTEM VALUE VALUES(
$1,$2,$3,$4,$5,COALESCE($6,(SELECT uid FROM issues WHERE id=$5)),
COALESCE($7,(SELECT uid FROM projects WHERE id=$4)),$8,$9,$10,$11,$12,$13,$14,
$15,$16,$17,$18,$19,$20,$21
)`, purge.ID, purge.UID, purge.OriginInstanceUID, purge.ProjectID, purge.PurgedIssueID,
		purge.IssueUID, purge.ProjectUID, projectName, purge.ShortID, purge.IssueTitle,
		purge.IssueAuthor, purge.CommentCount, purge.LinkCount, purge.LabelCount,
		purge.EventCount, purge.EventsDeletedMinID, purge.EventsDeletedMaxID,
		purge.PurgeResetAfterEventID, purge.Actor, purge.Reason, purge.PurgedAt)
	return pgReplayError(db.ImportKindPurgeLog, err)
}

func pgReplayProjectPurgeLog(
	ctx context.Context,
	tx *sql.Tx,
	purge *db.ProjectPurgeLogExport,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_purge_log(
id,uid,origin_instance_uid,project_id,project_uid,project_name,issue_count,event_count,
alias_count,comment_count,link_count,label_count,claim_count,pending_claim_request_count,
events_deleted_min_id,events_deleted_max_id,purge_reset_after_event_id,actor,reason,purged_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		purge.ID, purge.UID, purge.OriginInstanceUID, purge.ProjectID, purge.ProjectUID,
		purge.ProjectName, purge.IssueCount, purge.EventCount, purge.AliasCount,
		purge.CommentCount, purge.LinkCount, purge.LabelCount, purge.ClaimCount,
		purge.PendingClaimRequestCount, purge.EventsDeletedMinID, purge.EventsDeletedMaxID,
		purge.PurgeResetAfterEventID, purge.Actor, purge.Reason, purge.PurgedAt)
	return pgReplayError(db.ImportKindProjectPurgeLog, err)
}

func pgReplayProjectName(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	fallback string,
) (string, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=$1`, projectID).Scan(&name)
	if err == nil {
		return name, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("project %d not imported before project snapshot: %w",
		projectID, mapSQLError(err, nil))
}

func pgReplayEnsureSystemProject(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM projects WHERE uid=$1 AND name=$2
)`, db.SystemProjectUID, db.SystemProjectName).Scan(&exists); err != nil {
		return fmt.Errorf("check system project after import: %w", mapSQLError(err, nil))
	}
	if exists {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO projects(id,uid,name)
OVERRIDING SYSTEM VALUE
SELECT COALESCE(MAX(id),0)+1,$1,$2 FROM projects`, db.SystemProjectUID, db.SystemProjectName)
	if err != nil {
		return fmt.Errorf("ensure system project after import: %w", mapSQLError(err, nil))
	}
	return nil
}

type replayTokenEvent struct {
	id           int64
	typ          string
	payload      string
	createdAt    string
	eventProject string
	projectName  string
	projectUID   string
}

func (s *Store) replayAPITokens(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens`); err != nil {
		return fmt.Errorf("clear api_tokens projection: %w", mapSQLError(err, nil))
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.type,e.payload,e.created_at,
e.project_name,p.name,p.uid FROM events e JOIN projects p ON p.id=e.project_id
WHERE e.type IN ('token.created','token.revoked') ORDER BY e.id ASC`)
	if err != nil {
		return fmt.Errorf("read token events: %w", mapSQLError(err, nil))
	}
	var events []replayTokenEvent
	for rows.Next() {
		var event replayTokenEvent
		if err := rows.Scan(&event.id, &event.typ, &event.payload, &event.createdAt,
			&event.eventProject, &event.projectName, &event.projectUID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan token event: %w", mapSQLError(err, nil))
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read token event rows: %w", mapSQLError(err, nil))
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close token event rows: %w", mapSQLError(err, nil))
	}

	for _, event := range events {
		if event.projectUID != db.SystemProjectUID || event.projectName != db.SystemProjectName ||
			event.eventProject != db.SystemProjectName {
			return fmt.Errorf("%s event %d must belong to system project %s",
				event.typ, event.id, db.SystemProjectName)
		}
		switch event.typ {
		case "token.created":
			record, err := db.DecodeReplayTokenCreated([]byte(event.payload))
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO api_tokens(
id,token_hash,actor,name,created_at
) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5)`, record.TokenID,
				record.TokenHash, record.TargetActor, record.Name, event.createdAt)
			if err != nil {
				return fmt.Errorf("replay token.created %d: %w", record.TokenID, mapSQLError(err, nil))
			}
		case "token.revoked":
			record, err := db.DecodeReplayTokenRevoked([]byte(event.payload))
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `UPDATE api_tokens
SET revoked_at=COALESCE(revoked_at,$1) WHERE id=$2`, event.createdAt, record.TokenID)
			if err != nil {
				return fmt.Errorf("replay token.revoked %d: %w", record.TokenID, mapSQLError(err, nil))
			}
			rowsAffected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("replay token.revoked rows affected: %w", err)
			}
			if rowsAffected == 0 {
				return fmt.Errorf("replay token.revoked %d: token not found", record.TokenID)
			}
		}
	}
	return nil
}

func pgReplayRecordSchemaVersion(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version',$1)
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, strconv.Itoa(db.CurrentSchemaVersion()))
	if err != nil {
		return fmt.Errorf("record import schema version: %w", mapSQLError(err, nil))
	}
	return nil
}

var replayIdentityTables = []string{
	"projects",
	"project_aliases",
	"recurrences",
	"issues",
	"comments",
	"links",
	"events",
	"api_tokens",
	"purge_log",
	"project_purge_log",
	"issue_sync_bindings",
	"federation_quarantine",
	"federation_enrollments",
	"issue_claims",
	"pending_claim_requests",
	"import_mappings",
}

func (s *Store) reconcileReplayIdentities(
	ctx context.Context,
	tx *sql.Tx,
	floors map[string]int64,
) error {
	for _, table := range replayIdentityTables {
		var maxID int64
		query := `SELECT COALESCE(MAX(id),0) FROM ` + quoteIdentifier(table)
		if err := tx.QueryRowContext(ctx, query).Scan(&maxID); err != nil {
			return fmt.Errorf("max id for %s: %w", table, mapSQLError(err, nil))
		}
		floor := floors[table]
		if maxID > floor {
			floor = maxID
		}
		if floor == 0 {
			continue
		}
		qualifiedTable := quoteIdentifier(s.schema) + "." + quoteIdentifier(table)
		var sequence sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,'id')`, qualifiedTable).
			Scan(&sequence); err != nil {
			return fmt.Errorf("resolve identity sequence for %s: %w", table, mapSQLError(err, nil))
		}
		if !sequence.Valid || sequence.String == "" {
			return fmt.Errorf("identity sequence for %s.id not found", table)
		}
		if _, err := tx.ExecContext(ctx, `SELECT setval($1::regclass,$2,true)`, sequence.String, floor); err != nil {
			return fmt.Errorf("advance identity sequence for %s: %w", table, mapSQLError(err, nil))
		}
	}
	return nil
}
