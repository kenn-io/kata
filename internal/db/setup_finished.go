package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"uuid"
)

// SetupFinishedDelivery preserves the first visible issue's event across daemon restarts.
type SetupFinishedDelivery struct {
	InstanceUID string    `json:"instance_uid"`
	ID          uuid.UUID `json:"id"`
	Time        time.Time `json:"time"`
}

// VisibleIssuesSQL applies the shared setup and project-stat visibility rules.
const VisibleIssuesSQL = `SELECT EXISTS (
 SELECT 1 FROM issues i JOIN projects p ON p.id=i.project_id
 WHERE i.deleted_at IS NULL AND i.status IN ('open','closed')
 AND p.deleted_at IS NULL AND p.name <> $1)`

// PrepareSetupFinishedTx uses the current installation read by the backend's write transaction.
func PrepareSetupFinishedTx(ctx context.Context, tx *sql.Tx, uid string, observed time.Time) (*SetupFinishedDelivery, bool, error) {
	var finished string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='setup_finished'`).Scan(&finished)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if finished == uid {
		return nil, true, nil
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='setup_finished_pending'`).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if err == nil {
		var saved SetupFinishedDelivery
		if err := json.Unmarshal([]byte(raw), &saved); err != nil {
			return nil, false, fmt.Errorf("decode setup delivery: %w", err)
		}
		if saved.InstanceUID == "" || saved.ID == uuid.Nil() || saved.Time.IsZero() {
			return nil, false, errors.New("invalid setup delivery")
		}
		if saved.InstanceUID == uid {
			return &saved, false, nil
		}
	}
	var visible bool
	if err := tx.QueryRowContext(ctx, VisibleIssuesSQL, SystemProjectName).Scan(&visible); err != nil {
		return nil, false, err
	}
	if !visible {
		return nil, false, nil
	}
	saved := SetupFinishedDelivery{InstanceUID: uid, ID: uuid.New(), Time: observed.UTC()}
	if saved.Time.IsZero() {
		return nil, false, errors.New("setup observation time is required")
	}
	encoded, err := json.Marshal(saved)
	if err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('setup_finished_pending',$1)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(encoded)); err != nil {
		return nil, false, err
	}
	return &saved, false, nil
}

// CompleteSetupFinishedTx acknowledges only the saved delivery for the current install.
func CompleteSetupFinishedTx(ctx context.Context, tx *sql.Tx, uid string, delivery SetupFinishedDelivery) error {
	if uid != delivery.InstanceUID {
		return errors.New("setup installation changed")
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='setup_finished_pending'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		var finished string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='setup_finished'`).Scan(&finished); err != nil {
			return err
		}
		if finished == uid {
			return nil
		}
		return errors.New("setup delivery is missing")
	}
	if err != nil {
		return err
	}
	var saved SetupFinishedDelivery
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return err
	}
	if saved.InstanceUID != delivery.InstanceUID || saved.ID != delivery.ID || !saved.Time.Equal(delivery.Time) {
		return errors.New("setup delivery changed")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('setup_finished',$1)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, uid); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key='setup_finished_pending'`)
	return err
}
