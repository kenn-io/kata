package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

type cronLifecycleQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type cronEventQuery interface {
	cronLifecycleQuery
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// DeleteCronProject removes the project's cron projections during purge.
func DeleteCronProject(ctx context.Context, q cronLifecycleQuery, projectID int64) error {
	for _, table := range []string{"cron_runs", "cron_jobs", "cron_workflows"} {
		if _, err := q.ExecContext(ctx, "DELETE FROM "+table+" WHERE project_id=$1", projectID); err != nil {
			return err
		}
	}
	return nil
}

// MergeCronProjects moves cron projections in the project merge transaction.
func MergeCronProjects(ctx context.Context, tx *sql.Tx, source, target Project) error {
	if err := rebindCronEventProjectUIDs(ctx, tx, source, target); err != nil {
		return err
	}
	for _, table := range []string{"cron_jobs", "cron_workflows", "cron_runs"} {
		//nolint:gosec // Table identifier comes from the fixed three-table list; values are bound.
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET project_id=$1 WHERE project_id=$2", target.ID, source.ID); err != nil {
			return err
		}
	}
	return nil
}

func rebindCronEventProjectUIDs(ctx context.Context, q cronEventQuery, source, target Project) error {
	rows, err := q.QueryContext(ctx, "SELECT id, payload FROM events WHERE project_id=$1 AND type LIKE 'cron.%' ORDER BY id", source.ID)
	if err != nil {
		return fmt.Errorf("list cron events for project merge: %w", err)
	}
	type eventPayload struct {
		id  int64
		raw string
	}
	var events []eventPayload
	for rows.Next() {
		var event eventPayload
		if err := rows.Scan(&event.id, &event.raw); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan cron event for project merge: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate cron events for project merge: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close cron event rows for project merge: %w", err)
	}

	for _, event := range events {
		var payload map[string]jsontext.Value
		if err := json.Unmarshal([]byte(event.raw), &payload); err != nil {
			return fmt.Errorf("decode cron event %d for project merge: %w", event.id, err)
		}
		if payload == nil {
			return fmt.Errorf("decode cron event %d for project merge: expected JSON object", event.id)
		}
		uid, err := json.Marshal(target.UID)
		if err != nil {
			return fmt.Errorf("encode target project UID for cron event %d: %w", event.id, err)
		}
		payload["project_uid"] = jsontext.Value(uid)
		updated, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode cron event %d for project merge: %w", event.id, err)
		}
		if _, err := q.ExecContext(ctx, "UPDATE events SET payload=$1 WHERE id=$2", string(updated), event.id); err != nil {
			return fmt.Errorf("rebind cron event %d to target project: %w", event.id, err)
		}
	}
	return nil
}

// LockCronProject serializes ordinary project mutations before their
// binding and credential fences, using the same ordering on both backends.
func LockCronProject(ctx context.Context, tx Transaction, projectID int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=$1`, projectID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}
