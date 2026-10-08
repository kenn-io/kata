package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

const purgeLogSelect = `SELECT id, uid, origin_instance_uid, project_id, purged_issue_id, issue_uid,
       project_uid, project_name, short_id, issue_title, issue_author, comment_count, link_count,
       label_count, event_count, events_deleted_min_id, events_deleted_max_id,
       purge_reset_after_event_id, actor, reason, purged_at FROM purge_log`

// PurgeIssue atomically removes one issue and its dependent state, leaving a
// durable audit tombstone and an event-stream reset cursor.
func (s *Store) PurgeIssue(ctx context.Context, issueID int64, actor string, reason *string) (db.PurgeLog, error) {
	var result db.PurgeLog
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1 FOR UPDATE OF i`, issueID))
		if err != nil {
			return err
		}
		var projectName string
		if err := tx.QueryRowContext(ctx,
			`SELECT name FROM projects WHERE id = $1`, issue.ProjectID).Scan(&projectName); err != nil {
			return mapSQLError(err, nil)
		}
		if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, issue.ProjectID); err != nil {
			return err
		}
		if err := rejectActiveExternalRootIssuePurge(ctx, tx, issue.ID); err != nil {
			return err
		}
		// Keep manifests downloadable until the current live peer namespace
		// drains. Retired epochs and revoked enrollments remain history.
		var pendingRelay bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox o JOIN federation_enrollments e ON e.relay_binding_uid=o.binding_uid AND e.relay_reset_epoch=o.reset_epoch WHERE o.project_uid=$1 AND o.acknowledged=0 AND e.revoked_at IS NULL) OR EXISTS(SELECT 1 FROM federation_enrollments e JOIN meta m ON m.key=$2 || e.relay_binding_uid WHERE e.project_id=$3 AND e.revoked_at IS NULL AND (m.value::jsonb->'translation'->'authority'->>'epoch')::bigint>e.relay_reset_epoch)`, issue.ProjectUID, db.RelayResetMetadataPrefix+issue.ProjectUID+".", issue.ProjectID).Scan(&pendingRelay); err != nil {
			return err
		}
		if pendingRelay {
			return db.ErrFederationResetBlockedByPendingPush
		}

		var minEventID, maxEventID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(id), MAX(id) FROM events
          WHERE issue_id = $1 OR (related_issue_id = $1 AND type <> 'issue.links_changed')`, issue.ID,
		).Scan(&minEventID, &maxEventID); err != nil {
			return mapSQLError(err, nil)
		}
		counts := make([]int64, 4)
		queries := []string{
			`SELECT COUNT(*) FROM comments WHERE issue_id = $1`,
			`SELECT COUNT(*) FROM links WHERE from_issue_id = $1 OR to_issue_id = $1`,
			`SELECT COUNT(*) FROM issue_labels WHERE issue_id = $1`,
			`SELECT COUNT(*) FROM events WHERE issue_id = $1 OR (related_issue_id = $1 AND type <> 'issue.links_changed')`,
		}
		for index, query := range queries {
			if err := tx.QueryRowContext(ctx, query, issue.ID).Scan(&counts[index]); err != nil {
				return mapSQLError(err, nil)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM events
          WHERE issue_id = $1 OR (related_issue_id = $1 AND type <> 'issue.links_changed')`, issue.ID); err != nil {
			return mapSQLError(err, nil)
		}
		_, err = tx.ExecContext(ctx, `UPDATE events SET related_issue_id = NULL
		  WHERE related_issue_id = $1 AND type = 'issue.links_changed'`, issue.ID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		for _, statement := range []string{
			`DELETE FROM comments WHERE issue_id = $1`,
			`DELETE FROM links WHERE from_issue_id = $1 OR to_issue_id = $1`,
			`DELETE FROM issue_labels WHERE issue_id = $1`,
			`DELETE FROM pending_claim_requests WHERE issue_id = $1`,
			`DELETE FROM issue_claims WHERE issue_id = $1`,
		} {
			if _, err := tx.ExecContext(ctx, statement, issue.ID); err != nil {
				return mapSQLError(err, nil)
			}
		}
		if issue.RecurrenceID != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE recurrences
SET last_materialized_uid = (
      SELECT uid FROM issues
       WHERE recurrence_id = $1 AND id <> $2 AND occurrence_key IS NOT NULL
       ORDER BY occurrence_key DESC, id DESC LIMIT 1
    ),
    revision = revision + 1,
    updated_at = $3
WHERE id = $1 AND last_materialized_uid = $4`,
				*issue.RecurrenceID, issue.ID, nowStoredTimestamp(), issue.UID,
			); err != nil {
				return mapSQLError(err, nil)
			}
		}

		var resetCursor sql.NullInt64
		if minEventID.Valid {
			if err := lockEventSequenceTx(ctx, tx); err != nil {
				return err
			}
			value, err := s.reserveIdentityValue(ctx, tx, "events", "id")
			if err != nil {
				return err
			}
			resetCursor = sql.NullInt64{Int64: value, Valid: true}
		}
		purgeUID, err := katauid.New()
		if err != nil {
			return fmt.Errorf("generate purge uid: %w", err)
		}
		var purgeID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO purge_log(
          uid, origin_instance_uid, project_id, purged_issue_id, issue_uid, project_uid,
          project_name, short_id, issue_title, issue_author, comment_count, link_count,
          label_count, event_count, events_deleted_min_id, events_deleted_max_id,
          purge_reset_after_event_id, actor, reason
        ) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
        RETURNING id`,
			purgeUID, s.instanceUID, issue.ProjectID, issue.ID, issue.UID, issue.ProjectUID,
			projectName, issue.ShortID, issue.Title, issue.Author, counts[0], counts[1], counts[2], counts[3],
			minEventID, maxEventID, resetCursor, actor, reason,
		).Scan(&purgeID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		// Portable vectors are issue data even when their input is stale.
		if _, err := tx.ExecContext(ctx, `DELETE FROM federation_embedding_artifacts WHERE project_uid=$1 AND issue_uid=$2`, issue.ProjectUID, issue.UID); err != nil {
			return mapSQLError(err, nil)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM issues WHERE id = $1`, issue.ID); err != nil {
			return mapSQLError(err, nil)
		}
		result, err = scanPurgeLog(tx.QueryRowContext(ctx, purgeLogSelect+` WHERE id = $1`, purgeID))
		return err
	})
	return result, err
}

func rejectActiveExternalRootIssuePurge(ctx context.Context, tx *sql.Tx, issueID int64) error {
	var active int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM external_root_bindings WHERE issue_id = $1 AND active = 1 LIMIT 1`,
		issueID,
	).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapSQLError(err, nil)
	}
	return db.ErrExternalRootContentOwned
}

func rejectActiveExternalRootProjectPurge(ctx context.Context, tx *sql.Tx, projectID int64) error {
	var active int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM external_root_bindings WHERE project_id = $1 AND active = 1 LIMIT 1`,
		projectID,
	).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapSQLError(err, nil)
	}
	return db.ErrExternalRootContentOwned
}

// PurgeResetCheck reports the newest reserved cursor beyond afterID.
func (s *Store) PurgeResetCheck(ctx context.Context, afterID, projectID int64) (int64, error) {
	var value sql.NullInt64
	query := `SELECT MAX(cursor) FROM (
      SELECT MAX(purge_reset_after_event_id) AS cursor FROM purge_log
       WHERE purge_reset_after_event_id IS NOT NULL AND purge_reset_after_event_id > $1`
	args := []any{afterID}
	if projectID != 0 {
		args = append(args, projectID)
		query += ` AND project_id = $2`
	}
	query += ` UNION ALL SELECT MAX(purge_reset_after_event_id) AS cursor FROM project_purge_log
       WHERE purge_reset_after_event_id IS NOT NULL AND purge_reset_after_event_id > $1`
	if projectID != 0 {
		query += ` AND project_id = $2`
	}
	query += `) resets`
	if err := s.QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
		return 0, mapSQLError(err, nil)
	}
	attributionReset, err := s.attributionUIResetAfter(ctx, afterID, projectID)
	if err != nil {
		return 0, err
	}
	return max(value.Int64, attributionReset), nil
}

func scanPurgeLog(row rowScanner) (db.PurgeLog, error) {
	var result db.PurgeLog
	var purgedAt string
	err := row.Scan(
		&result.ID, &result.UID, &result.OriginInstanceUID, &result.ProjectID, &result.PurgedIssueID,
		&result.IssueUID, &result.ProjectUID, &result.ProjectName, &result.ShortID, &result.IssueTitle,
		&result.IssueAuthor, &result.CommentCount, &result.LinkCount, &result.LabelCount, &result.EventCount,
		&result.EventsDeletedMinID, &result.EventsDeletedMaxID, &result.PurgeResetAfterEventID,
		&result.Actor, &result.Reason, &purgedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PurgeLog{}, db.ErrNotFound
	}
	if err != nil {
		return db.PurgeLog{}, mapSQLError(err, nil)
	}
	result.PurgedAt, err = parseStoredTime(purgedAt)
	if err != nil {
		return db.PurgeLog{}, fmt.Errorf("parse purge timestamp: %w", err)
	}
	return result, nil
}
