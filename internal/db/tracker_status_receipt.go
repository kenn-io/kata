package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
)

// TrackerStatusReceiptStore recovers only retained definite responses. It never
// upgrades an unknown response from item readback or operator assertion.
type TrackerStatusReceiptStore interface {
	AcknowledgeRetainedTrackerStatus(context.Context, IssueSyncImportGuard) error
}

// AcknowledgeRetainedStatusInTransaction retires a historically delivered event
// without applying an old observation over newer provider/local state. The
// original completed response is retained in semantic history; a later intent
// remains pending. Current claim, binding and project fences come from the
// native statusReadBindingTx in this same transaction.
func (a TrackerFenceSQL) AcknowledgeRetainedStatusInTransaction(ctx context.Context, tx *sql.Tx, binding IssueSyncBinding) error {
	selection, err := IssueSyncConnectionFromConfig(binding.Config)
	if err != nil {
		return err
	}
	if selection == nil {
		return nil
	}
	if err := ValidateIssueSyncConnectionMarker(binding.ConnectionUID, binding.Config); err != nil {
		return err
	}
	var project string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, binding.ProjectID).Scan(&project); err != nil {
		return err
	}
	if err := a.authorize(ctx, tx, project); err != nil {
		return err
	}
	key, _ := TrackerExecutionFenceKey(a.InstanceUID, project, "coordinator")
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var head TrackerExecutionFence
	if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &head, json.RejectUnknownMembers(true)) != nil || head.Validate() != nil || !trackerKeyMatches(key, head) {
		return ErrTrackerFenceInvalid
	}
	if head.Phase != TrackerWriteConfirmed {
		return nil
	}
	if head.Connection != *selection || head.SourceKey != binding.SourceKey {
		return ErrIssueSyncBindingChanged
	}
	var receipt TrackerStatusResult
	if json.Unmarshal(head.Result, &receipt, json.RejectUnknownMembers(true)) != nil || receipt.Status != head.Target || receipt.Version.IsZero() {
		return ErrTrackerFenceInvalid
	}
	// Clearing only the historical UID neither overwrites current issue status
	// nor consumes a newer event. The existing provider observation is untouched.
	if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=NULL WHERE project_id=$1 AND source=$2 AND external_id=$3 AND pending_event_uid=$4`, binding.ProjectID, binding.SourceKey, head.Locator, head.PendingEventUID); err != nil {
		return err
	}
	next := head
	next.Phase = TrackerWriteAcknowledged
	return a.StoreInTransaction(ctx, tx, &head, next, head.ProcessEpoch)
}

// Metadata recovery consumes only a previously confirmed original response.
// It changes no issue/observation, requires no run claim or provider permission,
// and never grants replica storage authority.
type TrackerRecoveryReceiptStore interface {
	AcknowledgeTrackerRecovery(context.Context, string) error
}

func (a TrackerFenceSQL) AcknowledgeRecovery(ctx context.Context, project string) error {
	return a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, project); err != nil {
			return err
		}
		var spoke int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_bindings f JOIN projects p ON p.id=f.project_id WHERE p.uid=$1 AND f.role='spoke' AND f.enabled=1`, project).Scan(&spoke); err != nil {
			return err
		}
		if spoke != 0 {
			return ErrIssueSyncFederationBinding
		}
		var b IssueSyncBinding
		var raw string
		var marker sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT b.id,b.project_id,b.provider,b.source_key,b.config_json,b.connection_uid FROM issue_sync_bindings b JOIN projects p ON p.id=b.project_id WHERE p.uid=$1 AND p.deleted_at IS NULL`, project).Scan(&b.ID, &b.ProjectID, &b.Provider, &b.SourceKey, &raw, &marker)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		b.Config = []byte(raw)
		if marker.Valid {
			b.ConnectionUID = &marker.String
		}
		return a.AcknowledgeRetainedStatusInTransaction(ctx, tx, b)
	})
}
