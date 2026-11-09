package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/kata/internal/uid"
)

type TrackerRebindParams struct {
	Binding           UpsertIssueSyncBindingParams
	ExpectedUpdatedAt time.Time
	ProcessEpoch      string
}
type TrackerRebindStore interface {
	RebindTrackerConnection(context.Context, TrackerRebindParams) (IssueSyncBinding, error)
}

// RebindInTransaction changes only a named binding at an explicitly fenced,
// settled root boundary. Provider/source identity remains immutable. Ordinary
// enable cannot invoke this path, and legacy bindings cannot assert that an old
// unfenced provider mutation stopped merely by acquiring a named marker.
func (a TrackerFenceSQL) RebindInTransaction(ctx context.Context, tx *sql.Tx, current IssueSyncBinding, p TrackerRebindParams) error {
	next := p.Binding
	if next.Connection == nil || next.Connection.Validate() != nil || next.ExpectedBinding == nil || next.ExpectedBinding.ID != current.ID || next.ProjectID != current.ProjectID || p.ExpectedUpdatedAt.IsZero() || !p.ExpectedUpdatedAt.Equal(current.UpdatedAt) || !uid.Valid(p.ProcessEpoch) {
		return ErrIssueSyncBindingChanged
	}
	matches, err := IssueSyncConfigMatches(current.Config, next.ExpectedBinding.Config)
	if err != nil {
		return err
	}
	if !matches || current.IntervalSeconds != next.ExpectedBinding.IntervalSeconds || current.Provider != next.Provider || current.SourceKey != next.SourceKey || current.RemoteID != next.RemoteID {
		return ErrIssueSyncBindingChanged
	}
	if err := ValidateIssueSyncConnectionMarker(current.ConnectionUID, current.Config); err != nil {
		return err
	}
	old, err := IssueSyncConnectionFromConfig(current.Config)
	if err != nil {
		return err
	}
	if old == nil {
		return ErrIssueSyncBindingChanged
	}
	var project string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, current.ProjectID).Scan(&project); err != nil {
		return err
	}
	if err := a.authorize(ctx, tx, project); err != nil {
		return err
	}
	// A definite original response may be acknowledged without the revoked old
	// credential. This preserves newer local intent and never promotes a read.
	if err := a.AcknowledgeRetainedStatusInTransaction(ctx, tx, current); err != nil {
		return err
	}
	key, _ := TrackerExecutionFenceKey(a.InstanceUID, project, "coordinator")
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
	if err == nil {
		var head TrackerExecutionFence
		if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &head, json.RejectUnknownMembers(true)) != nil || head.Validate() != nil || !trackerKeyMatches(key, head) {
			return ErrTrackerFenceInvalid
		}
		if !trackerFenceSettled(head) {
			return ErrTrackerOutcomeUnknown
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	config, err := PreserveIssueStatusScanConfig(current.Config, next.Config)
	if err != nil {
		return err
	}
	config, err = SetIssueSyncConnectionConfig(config, *next.Connection)
	if err != nil {
		return err
	}
	mode, err := IssueStatusMode(config)
	if err != nil {
		return err
	}
	if mode == "one-way" {
		if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=NULL WHERE project_id=$1 AND source=$2`, current.ProjectID, current.SourceKey); err != nil {
			return err
		}
	}
	updated := NextIssueSyncBindingUpdatedAt(current.UpdatedAt, time.Now()).UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE issue_sync_bindings SET config_json=$1,connection_uid=$2,display_name=$3,interval_seconds=$4,updated_at=$5,enabled=1,last_cursor_at=NULL WHERE id=$6 AND config_json=$7`, string(config), next.Connection.ConnectionUID, next.DisplayName, next.IntervalSeconds, updated, current.ID, string(current.Config))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrIssueSyncBindingChanged
	}
	// Existing run claims cannot authorize the replacement snapshot. The native
	// runner's updated_at guard rejects their later imports or provider admission.
	var floor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=$1`, current.ProjectID).Scan(&floor); err != nil {
		return err
	}
	gap, err := trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, project)
	if err != nil {
		return err
	}
	var priorRaw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, TrackerActivationKey(a.InstanceUID, project)).Scan(&priorRaw); err == nil {
		var prior TrackerActivation
		if len(priorRaw) > 8192 || json.Unmarshal([]byte(priorRaw), &prior, json.RejectUnknownMembers(true)) != nil || prior.Validate() != nil || prior.RootInstanceUID != a.InstanceUID || prior.ProjectUID != project {
			return ErrTrackerFenceInvalid
		}
		gap = gap || prior.RestoredHistoryGap
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return writeTrackerActivationSQL(ctx, tx, TrackerActivation{RestoredHistoryGap: gap, Version: 1, RootInstanceUID: a.InstanceUID, ProjectUID: project, Connection: *next.Connection, BindingID: current.ID, SourceKey: current.SourceKey, ProcessEpoch: p.ProcessEpoch, EventFloor: floor, Active: false})
}
