package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrFederationResetBlockedByPendingPush reports that reset would discard
// local-origin events not yet accepted by the hub.
var ErrFederationResetBlockedByPendingPush = errors.New("federation reset blocked by pending push")

// ErrFederationPushQuarantined reports that an unresolved poisoned push batch
// requires operator action before the push stream may continue.
var ErrFederationPushQuarantined = errors.New("federation push quarantined")

// ErrFederationResetBlockedByQuarantine reports that reset would discard local
// state while an operator-visible poisoned batch remains unresolved.
var ErrFederationResetBlockedByQuarantine = errors.New("federation reset blocked by quarantine")

// PendingFederationPushEvents returns local-origin events that have not yet
// been acknowledged by the hub for a push-enabled spoke binding.
func (d *DB) PendingFederationPushEvents(
	ctx context.Context,
	projectID int64,
	originInstanceUID string,
	afterID int64,
	limit int,
) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	out, err := d.queryPendingFederationPushEvents(ctx, `
		WHERE e.project_id = ?
		  AND e.origin_instance_uid = ?
		  AND e.id > ?
		  AND `+federationPushEventTypeCondition("e.type")+`
		ORDER BY e.id ASC
		LIMIT ?`, projectID, originInstanceUID, afterID, limit)
	if err != nil {
		return nil, err
	}
	if len(out) == limit && len(out) > 0 && out[len(out)-1].Type == "issue.snapshot" {
		runStartAfterID := afterID
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Type != "issue.snapshot" {
				runStartAfterID = out[i].ID
				break
			}
		}
		extra, err := d.queryPendingFederationPushEvents(ctx, `
			WHERE e.project_id = ?
			  AND e.origin_instance_uid = ?
			  AND e.id > ?
			  AND e.type = 'issue.snapshot'
			  AND NOT EXISTS (
			    SELECT 1
			      FROM events barrier
			     WHERE barrier.project_id = e.project_id
			       AND barrier.origin_instance_uid = e.origin_instance_uid
			       AND barrier.id > ?
			       AND barrier.id < e.id
			       AND `+federationPushEventTypeCondition("barrier.type")+`
			       AND barrier.type <> 'issue.snapshot'
			  )
			ORDER BY e.id ASC`, projectID, originInstanceUID, out[len(out)-1].ID, runStartAfterID)
		if err != nil {
			return nil, err
		}
		out = append(out, extra...)
	}
	for i, ev := range out {
		if ev.Type != "issue.snapshot" {
			continue
		}
		for j := i + 1; j < len(out); j++ {
			if out[j].Type != "issue.snapshot" {
				return out[:j], nil
			}
		}
		break
	}
	return out, nil
}

func (d *DB) queryPendingFederationPushEvents(ctx context.Context, where string, args ...any) ([]Event, error) {
	rows, err := d.QueryContext(ctx, `SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
	             e.issue_id, e.issue_uid, i.short_id, e.related_issue_id, e.related_issue_uid, ri.short_id,
	             e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at
	      FROM events e
	      JOIN projects p ON p.id = e.project_id
	      LEFT JOIN issues i ON i.project_id = e.project_id AND (i.id = e.issue_id OR (e.issue_id IS NULL AND e.issue_uid IS NOT NULL AND i.uid = e.issue_uid))
	      LEFT JOIN issues ri ON ri.project_id = e.project_id AND (ri.id = e.related_issue_id OR (e.related_issue_id IS NULL AND e.related_issue_uid IS NOT NULL AND ri.uid = e.related_issue_uid))
	      `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("pending federation push events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending federation push event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PendingFederationPushStats returns the pending count and high-water local
// events.id using the same supported-event filter as PendingFederationPushEvents.
func (d *DB) PendingFederationPushStats(
	ctx context.Context,
	projectID int64,
	originInstanceUID string,
	afterID int64,
) (int64, int64, error) {
	var count int64
	var maxID sql.NullInt64
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*), MAX(id)
		  FROM events
		 WHERE project_id = ?
		   AND origin_instance_uid = ?
		   AND id > ?
		   AND `+federationPushEventTypeCondition("type"),
		projectID, originInstanceUID, afterID).Scan(&count, &maxID); err != nil {
		return 0, 0, fmt.Errorf("count pending federation push: %w", err)
	}
	if maxID.Valid {
		return count, maxID.Int64, nil
	}
	return count, 0, nil
}

func federationPushEventTypeCondition(column string) string {
	return column + ` IN (
		'project.metadata_updated',
		'issue.created', 'issue.snapshot', 'issue.updated', 'issue.closed', 'issue.reopened',
		'issue.soft_deleted', 'issue.restored', 'issue.commented',
		'issue.assigned', 'issue.unassigned', 'issue.priority_set', 'issue.priority_cleared',
		'issue.labeled', 'issue.unlabeled',
		'issue.linked', 'issue.unlinked', 'issue.links_changed', 'issue.metadata_updated'
	)`
}

// AdvanceFederationPushCursor records the highest local events.id accepted by
// the hub for a spoke binding.
func (d *DB) AdvanceFederationPushCursor(ctx context.Context, projectID, nextCursor int64) error {
	res, err := d.ExecContext(ctx, `
		UPDATE federation_bindings
		   SET push_cursor_event_id = CASE
		         WHEN push_cursor_event_id < ? THEN ?
		         ELSE push_cursor_event_id
		       END,
		       last_sync_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE project_id = ?`,
		nextCursor, nextCursor, projectID)
	if err != nil {
		return fmt.Errorf("advance federation push cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance federation push cursor rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnableFederationPush marks an existing spoke binding push-enabled and keeps
// the cursor monotonic across idempotent setup retries.
func (d *DB) EnableFederationPush(ctx context.Context, projectID int64, cursor int64) (FederationBinding, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return FederationBinding{}, fmt.Errorf("begin enable federation push: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT push_cursor_event_id FROM federation_bindings WHERE project_id = ?`,
		projectID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return FederationBinding{}, ErrNotFound
	}
	if err != nil {
		return FederationBinding{}, fmt.Errorf("lookup federation push cursor: %w", err)
	}
	nextCursor := cursor
	if existing.Valid && existing.Int64 > nextCursor {
		nextCursor = existing.Int64
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE federation_bindings
		   SET push_enabled = 1,
		       push_cursor_event_id = ?,
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE project_id = ?`,
		nextCursor, projectID)
	if err != nil {
		return FederationBinding{}, fmt.Errorf("enable federation push: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return FederationBinding{}, fmt.Errorf("enable federation push rows affected: %w", err)
	}
	if n == 0 {
		return FederationBinding{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return FederationBinding{}, fmt.Errorf("commit enable federation push: %w", err)
	}
	return d.FederationBindingByProject(ctx, projectID)
}

// ResetFederatedProjectIfNoPendingPush clears a spoke only if no local-origin
// events remain above pushCursorEventID. The guarded UPDATE takes SQLite's
// write lock before projection/event deletion, so a concurrent local write
// cannot slip between the pending check and reset cleanup.
func (d *DB) ResetFederatedProjectIfNoPendingPush(
	ctx context.Context,
	projectID, replayHorizonEventID, pullCursorEventID int64,
	originInstanceUID string,
	pushCursorEventID int64,
) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin guarded federated reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE federation_bindings
		   SET replay_horizon_event_id = ?,
		       pull_cursor_event_id = ?,
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE project_id = ?
		   AND NOT EXISTS (
		       SELECT 1
		         FROM events
		        WHERE project_id = ?
		          AND origin_instance_uid = ?
		          AND id > ?
		          AND type IN (
		            'project.metadata_updated',
		            'issue.created', 'issue.snapshot', 'issue.updated', 'issue.closed', 'issue.reopened',
		            'issue.soft_deleted', 'issue.restored', 'issue.commented',
		            'issue.assigned', 'issue.unassigned', 'issue.priority_set', 'issue.priority_cleared',
		            'issue.labeled', 'issue.unlabeled',
		            'issue.linked', 'issue.unlinked', 'issue.links_changed', 'issue.metadata_updated'
		          )
		   )
		   AND NOT EXISTS (
		       SELECT 1
		         FROM federation_quarantine
		        WHERE project_id = ?
		          AND skipped_at IS NULL
		   )`,
		replayHorizonEventID, pullCursorEventID, projectID,
		projectID, originInstanceUID, pushCursorEventID,
		projectID)
	if err != nil {
		return fmt.Errorf("guard federation reset cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("guard federation reset cursor rows affected: %w", err)
	}
	if n == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM federation_bindings WHERE project_id = ?`, projectID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("lookup guarded federation reset binding: %w", err)
		}
		var activeQuarantine int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM federation_quarantine WHERE project_id = ? AND skipped_at IS NULL LIMIT 1`,
			projectID).Scan(&activeQuarantine); err == nil {
			return ErrFederationResetBlockedByQuarantine
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lookup guarded federation reset quarantine: %w", err)
		}
		return ErrFederationResetBlockedByPendingPush
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("clear federated events: %w", err)
	}
	if err := clearFederatedProjection(ctx, tx, projectID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit guarded federated reset: %w", err)
	}
	return nil
}
