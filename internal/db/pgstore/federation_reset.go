package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// ResetFederatedProject clears one replica projection and rewinds its pull boundary.
func (s *Store) ResetFederatedProject(
	ctx context.Context,
	projectID int64,
	replayHorizonEventID int64,
	pullCursorEventID int64,
) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if _, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID)); err != nil {
			return err
		}
		if err := rejectLegacyFederationResetTx(ctx, tx, projectID); err != nil {
			return err
		}
		if err := clearFederatedProjectTx(ctx, tx, projectID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET
replay_horizon_event_id=$1,pull_cursor_event_id=$2,
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$3`, replayHorizonEventID, pullCursorEventID, projectID)
		return mapSQLError(err, nil)
	})
}

// ResetFederatedProjectIfNoPendingPush clears one replica only after every
// supported local event is acknowledged and every quarantine is resolved.
func (s *Store) ResetFederatedProjectIfNoPendingPush(
	ctx context.Context,
	projectID int64,
	replayHorizonEventID int64,
	pullCursorEventID int64,
	originInstanceUID string,
	pushCursorEventID int64,
) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if _, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID)); err != nil {
			return err
		}
		if err := rejectLegacyFederationResetTx(ctx, tx, projectID); err != nil {
			return err
		}
		var quarantineID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM federation_quarantine
WHERE project_id=$1 AND skipped_at IS NULL ORDER BY id ASC LIMIT 1 FOR UPDATE`, projectID).
			Scan(&quarantineID)
		if err == nil {
			return db.ErrFederationResetBlockedByQuarantine
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapSQLError(err, nil)
		}
		var pending bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events
WHERE project_id=$1 AND origin_instance_uid=$2 AND id>$3 AND `+
			pgFederationPushEventTypeCondition("type")+`)`, projectID, originInstanceUID, pushCursorEventID).
			Scan(&pending)
		if err != nil {
			return mapSQLError(err, nil)
		}
		if pending {
			return db.ErrFederationResetBlockedByPendingPush
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET
replay_horizon_event_id=$1,pull_cursor_event_id=$2,
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$3`, replayHorizonEventID, pullCursorEventID, projectID); err != nil {
			return mapSQLError(err, nil)
		}
		return clearFederatedProjectTx(ctx, tx, projectID)
	})
}

func rejectLegacyFederationResetTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	var relayConfig *string
	if err := tx.QueryRowContext(ctx, `SELECT relay_config FROM federation_bindings WHERE project_id=$1`, projectID).Scan(&relayConfig); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		return mapSQLError(err, nil)
	}
	if relayConfig != nil {
		return db.ErrRelayResetRequiresRootProof
	}
	var bindingID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM external_root_bindings WHERE project_id=$1 LIMIT 1 FOR UPDATE`, projectID).Scan(&bindingID)
	if err == nil {
		return db.ErrFederationResetBlockedByExternalRoot
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return mapSQLError(err, nil)
}

// clearRelayProjectTx replaces a relay project's projection but keeps
// daemon-local catalog history, such as renames and alias removals, which has
// no root receipt and is not in the snapshot. Every other record, including
// anything a root receipt attests, is replaced.
func clearRelayProjectTx(ctx context.Context, tx *sql.Tx, projectID int64, projectUID string) error {
	// #nosec G202 -- The event-type list is fixed native SQL; the project values are bound.
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1 AND NOT (type IN (`+db.RelayResetRetainedEventTypesSQL()+`) AND issue_uid IS NULL AND issue_id IS NULL AND NOT EXISTS(SELECT 1 FROM federation_event_provenance p WHERE p.project_uid=$2 AND p.event_uid=events.uid))`, projectID, projectUID); err != nil {
		return mapSQLError(err, nil)
	}
	return clearFederatedProjectionTx(ctx, tx, projectID)
}

func clearFederatedProjectTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID); err != nil {
		return mapSQLError(err, nil)
	}
	return clearFederatedProjectionTx(ctx, tx, projectID)
}

func clearFederatedProjectionTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM cron_runs WHERE project_id=$1`, projectID); err != nil {
		return err
	}
	for _, table := range []string{"cron_jobs", "cron_workflows"} {
		//nolint:gosec // Table comes from this fixed list; project ID is bound.
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE project_id=$1", projectID); err != nil {
			return err
		}
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM pending_claim_requests WHERE project_id=$1`, []any{projectID}},
		{`DELETE FROM issue_claims WHERE project_id=$1`, []any{projectID}},
		{`DELETE FROM issue_labels WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1)`, []any{projectID}},
		{`DELETE FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1)`, []any{projectID}},
		{`DELETE FROM links
WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id=$1)
   OR to_issue_id IN (SELECT id FROM issues WHERE project_id=$1)`, []any{projectID}},
		{`DELETE FROM issues WHERE project_id=$1`, []any{projectID}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return mapSQLError(err, nil)
		}
	}
	return nil
}
