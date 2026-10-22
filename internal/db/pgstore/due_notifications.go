package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// ListDueNotificationIssueIDs returns live open issues that have a planning
// date or inbox metadata that may need automatic cleanup. Enabled spokes are
// projections of their hub and never originate these writes.
func (s *Store) ListDueNotificationIssueIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT i.id
		  FROM issues i
		  JOIN projects p ON p.id = i.project_id
		  LEFT JOIN federation_bindings fb ON fb.project_id = p.id
		 WHERE i.deleted_at IS NULL
		   AND p.deleted_at IS NULL
		   AND i.status = 'open'
		   AND NOT (COALESCE(fb.enabled, 0) = 1 AND fb.role = 'spoke')
		   AND (
			 (i.metadata)::jsonb ? 'scheduled_on'
			 OR (i.metadata)::jsonb ? 'deadline_on'
			 OR EXISTS (
				 SELECT 1 FROM jsonb_object_keys((i.metadata)::jsonb) AS entry(key)
				  WHERE entry.key LIKE 'notify.%'
			 )
		   )
		 ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("list due notification issues: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due notification issue: %w", mapSQLError(err, nil))
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due notification issues: %w", mapSQLError(err, nil))
	}
	return ids, nil
}

// ReconcileDueNotification atomically rereads one issue, plans an ordinary
// notify.* metadata patch from its dates and event history, and emits the same
// metadata event as a manual patch. A newly reached scheduled_on value is also
// recorded as issue.scheduled_reached, whether or not the inbox slot is free.
func (s *Store) ReconcileDueNotification(ctx context.Context, input db.ReconcileDueNotificationIn) (db.ReconcileDueNotificationOut, error) {
	var output db.ReconcileDueNotificationOut
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.ReconcileDueNotificationOut{}
		issue, recurrenceTimezone, err := scanScheduledIssue(ctx, tx.QueryRowContext(ctx,
			scheduledIssueSelect+` WHERE i.id = $1 AND i.deleted_at IS NULL AND p.deleted_at IS NULL FOR UPDATE OF i`,
			input.IssueID))
		if err != nil {
			return err
		}
		if issue.Status != "open" {
			return nil
		}
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, issue.ProjectID))
		if err != nil {
			return err
		}
		enabledSpoke, err := enabledSpokeProjectTx(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		if enabledSpoke {
			return nil
		}
		history, err := issueMetadataHistoryTx(ctx, tx, issue.ID, issue.UID)
		if err != nil {
			return err
		}
		owner := ""
		if issue.Owner != nil {
			owner = *issue.Owner
		}
		scheduledTimezone := scheduleDefaultTimezone(recurrenceTimezone, input.DefaultTimezone)
		var reached *db.IssuePlanningDate
		if floor := input.ScheduledReachedFloor; floor != nil {
			if reached, err = db.PlanScheduledReached(jsontext.Value(issue.Metadata), input.Now, *floor, scheduledTimezone, history); err != nil {
				return err
			}
		}
		patch, err := db.PlanDueNotification(
			jsontext.Value(issue.Metadata), owner, issue.Author, input.Now,
			input.DefaultTimezone, scheduledTimezone, history,
		)
		if err != nil {
			// The reached event needs no recipient, so record it regardless.
			output.NotificationErr, patch = err, nil
		}
		if reached != nil {
			payload, err := json.Marshal(reached)
			if err != nil {
				return fmt.Errorf("marshal scheduled reached event: %w", err)
			}
			event, err := s.insertEventTx(ctx, tx,
				issueEventInput(issue, project, db.ScheduledReachedEventType, "system", string(payload)))
			if err != nil {
				return err
			}
			output.Reached = &event
			output.Changed = true
		}
		if len(patch) == 0 {
			return nil
		}
		for key, raw := range patch {
			if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
				return fmt.Errorf("validate %q: %w", key, err)
			}
		}
		updated, diff, err := patchedMetadata(issue.Metadata, patch)
		if err != nil {
			return err
		}
		if len(diff) == 0 {
			return nil
		}
		updatedAt := mutationTimestamp()
		newRevision := issue.Revision + 1
		if _, err := tx.ExecContext(ctx, `UPDATE issues
			SET metadata = $1, revision = $2, updated_at = $3 WHERE id = $4`,
			string(updated), newRevision, updatedAt, issue.ID); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			Diff        map[string]metadataKeyDiffPayload `json:"diff"`
			RevisionNew int64                             `json:"revision_new"`
			UpdatedAt   string                            `json:"updated_at"`
		}{Diff: diff, RevisionNew: newRevision, UpdatedAt: updatedAt})
		if err != nil {
			return fmt.Errorf("marshal due notification event: %w", err)
		}
		event, err := s.insertEventTx(ctx, tx,
			issueEventInput(issue, project, "issue.metadata_updated", "system", string(payload)))
		if err != nil {
			return err
		}
		output.Event = &event
		output.Changed = true
		return nil
	})
	return output, err
}

func enabledSpokeProjectTx(ctx context.Context, tx *sql.Tx, projectID int64) (bool, error) {
	var role string
	var enabled int
	err := tx.QueryRowContext(ctx,
		`SELECT role, enabled FROM federation_bindings WHERE project_id = $1`, projectID,
	).Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check due notification federation role: %w", mapSQLError(err, nil))
	}
	return enabled == 1 && role == string(db.FederationRoleSpoke), nil
}

func issueMetadataHistoryTx(ctx context.Context, tx *sql.Tx, issueID int64, issueUID string) ([]db.Event, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT uid, origin_instance_uid, type, payload, hlc_physical_ms, hlc_counter, created_at
		  FROM events
		 WHERE (issue_id = $1 OR (issue_id IS NULL AND issue_uid = $2))
		   AND type IN ('issue.created', 'issue.snapshot', 'issue.metadata_updated', 'issue.scheduled_reached', 'recurrence.materialized')
		 ORDER BY hlc_physical_ms, hlc_counter, origin_instance_uid, uid`, issueID, issueUID)
	if err != nil {
		return nil, fmt.Errorf("read due notification history: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var history []db.Event
	for rows.Next() {
		var event db.Event
		var createdAt string
		if err := rows.Scan(&event.UID, &event.OriginInstanceUID, &event.Type, &event.Payload,
			&event.HLCPhysicalMS, &event.HLCCounter, &createdAt); err != nil {
			return nil, fmt.Errorf("scan due notification history: %w", mapSQLError(err, nil))
		}
		if event.CreatedAt, err = parseStoredTime(createdAt); err != nil {
			return nil, fmt.Errorf("parse due notification history time: %w", err)
		}
		history = append(history, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due notification history: %w", mapSQLError(err, nil))
	}
	return history, nil
}

// EnsureScheduledReachedFloor stores at as the reached-schedule floor on the
// first call and returns the stored floor on every call.
func (s *Store) EnsureScheduledReachedFloor(ctx context.Context, at time.Time) (time.Time, error) {
	var floor time.Time
	err := s.withTx(ctx, sql.LevelDefault, func(tx *sql.Tx) error {
		var err error
		floor, err = db.EnsureScheduledReachedFloorTx(ctx, tx, at)
		return err
	})
	return floor, err
}
