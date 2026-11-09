package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"go.kenn.io/kata/internal/uid"
)

var ErrTrackerActivationRequired = errors.New("tracker connection requires explicit activation and reconciliation")

// TrackerActivation exists independently of writes, including on restored named
// bindings whose backup predates the first dispatch. EventFloor prevents old
// pending intents from becoming fresh outbound work after activation.
type TrackerActivation struct {
	RestoredHistoryGap bool                `json:"restored_history_gap,omitempty"`
	Version            int                 `json:"version"`
	RootInstanceUID    string              `json:"root_instance_uid"`
	ProjectUID         string              `json:"project_uid"`
	Connection         IssueSyncConnection `json:"connection"`
	BindingID          int64               `json:"binding_id"`
	SourceKey          string              `json:"source_key"`
	ProcessEpoch       string              `json:"process_epoch"`
	Active             bool                `json:"active"`
	EventFloor         int64               `json:"event_floor"`
}

func (a TrackerActivation) Validate() error {
	if a.Version != 1 || !uid.Valid(a.RootInstanceUID) || !uid.Valid(a.ProjectUID) || a.Connection.Validate() != nil || a.BindingID <= 0 || (a.Active && a.RestoredHistoryGap) || !trackerIdentityText(a.SourceKey) || !uid.Valid(a.ProcessEpoch) || a.EventFloor < 0 {
		return ErrTrackerFenceInvalid
	}
	return nil
}
func (a TrackerActivation) Allows(epoch string, bindingID int64, selection IssueSyncConnection, source string, eventID int64) bool {
	return a.Validate() == nil && a.Active && !a.RestoredHistoryGap && a.ProcessEpoch == epoch && a.BindingID == bindingID && a.Connection == selection && a.SourceKey == source && eventID > a.EventFloor
}
func TrackerActivationKey(root, project string) string {
	return TrackerMetadataPrefix + "activation." + project + "." + root
}

type TrackerActivationStore interface {
	LoadTrackerActivation(context.Context, string) (TrackerActivation, bool, error)
	ActivateTrackerBinding(context.Context, int64, time.Time, string) (TrackerActivation, error)
}

func (a TrackerFenceSQL) LoadActivation(ctx context.Context, projectUID string) (TrackerActivation, bool, error) {
	var record TrackerActivation
	found := false
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		record, found = TrackerActivation{}, false
		if err := a.authorize(ctx, tx, projectUID); err != nil {
			return err
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, TrackerActivationKey(a.InstanceUID, projectUID)).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(raw) > 8192 || json.Unmarshal([]byte(raw), &record, json.RejectUnknownMembers(true)) != nil || record.Validate() != nil || record.RootInstanceUID != a.InstanceUID || record.ProjectUID != projectUID {
			return ErrTrackerFenceInvalid
		}
		found = true
		return nil
	})
	return record, found, err
}

// Activate is called only after the daemon's live policy and provider source /
// status reconciliation. It fences that work against the supplied binding time
// and refuses activation over an unresolved dispatch. It never clears history.
func (a TrackerFenceSQL) Activate(ctx context.Context, bindingID int64, updatedAt time.Time, epoch string) (TrackerActivation, error) {
	if bindingID <= 0 || !uid.Valid(epoch) {
		return TrackerActivation{}, ErrTrackerFenceInvalid
	}
	var record TrackerActivation
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		var projectUID, source, config, storedUpdated string
		var marker *string
		var enabled int
		err := tx.QueryRowContext(ctx, `SELECT p.uid,b.source_key,b.config_json,b.connection_uid,b.enabled,CAST(b.updated_at AS TEXT) FROM issue_sync_bindings b JOIN projects p ON p.id=b.project_id WHERE b.id=$1 AND p.deleted_at IS NULL`, bindingID).Scan(&projectUID, &source, &config, &marker, &enabled, &storedUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := a.authorize(ctx, tx, projectUID); err != nil {
			return err
		}
		if blocked, err := trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, projectUID); err != nil {
			return err
		} else if blocked {
			return ErrTrackerOutcomeUnknown
		}
		var priorActivationRaw string
		priorErr := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, TrackerActivationKey(a.InstanceUID, projectUID)).Scan(&priorActivationRaw)
		if priorErr == nil {
			var prior TrackerActivation
			if len(priorActivationRaw) > 8192 || json.Unmarshal([]byte(priorActivationRaw), &prior, json.RejectUnknownMembers(true)) != nil || prior.Validate() != nil || prior.RootInstanceUID != a.InstanceUID || prior.ProjectUID != projectUID {
				return ErrTrackerFenceInvalid
			}
			if prior.RestoredHistoryGap {
				return ErrTrackerOutcomeUnknown
			}
		} else if !errors.Is(priorErr, sql.ErrNoRows) {
			return priorErr
		}
		if enabled != 1 {
			return ErrIssueSyncNotEnabled
		}
		if err := ValidateIssueSyncConnectionMarker(marker, []byte(config)); err != nil {
			return err
		}
		selection, err := IssueSyncConnectionFromConfig([]byte(config))
		if err != nil || selection == nil {
			return ErrTrackerFenceInvalid
		}
		var actual time.Time
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
			if parsed, err := time.Parse(layout, storedUpdated); err == nil {
				actual = parsed
				break
			}
		}
		if !actual.Equal(updatedAt) {
			return ErrIssueSyncBindingChanged
		}
		var spoke int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_bindings f JOIN projects p ON p.id=f.project_id WHERE p.uid=$1 AND f.role='spoke' AND f.enabled=1`, projectUID).Scan(&spoke); err != nil {
			return err
		}
		if spoke != 0 {
			return ErrIssueSyncFederationBinding
		}
		key, _ := TrackerExecutionFenceKey(a.InstanceUID, projectUID, "coordinator")
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
		if err == nil {
			var fence TrackerExecutionFence
			if json.Unmarshal([]byte(raw), &fence, json.RejectUnknownMembers(true)) != nil || fence.Validate() != nil || !trackerKeyMatches(key, fence) {
				return ErrTrackerFenceInvalid
			}
			if !trackerFenceSettled(fence) {
				return ErrTrackerOutcomeUnknown
			}
			// Only a settled head can cross an explicit activation boundary.
			// Its semantic history remains untouched, including restored evidence.
			fence.ActivationRequired = false
			// Remote reservations retain their original immutable root incarnation.
			// A new sequence captures the new epoch; old receipts remain verifiable.
			if fence.Remote == nil {
				fence.ProcessEpoch = epoch
			}
			encoded, err := json.Marshal(fence)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`, string(encoded), key, raw)
			if err != nil {
				return err
			}
			if changed, err := result.RowsAffected(); err != nil {
				return err
			} else if changed != 1 {
				return ErrTrackerFenceChanged
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var floor int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(e.id),0) FROM events e JOIN projects p ON p.id=e.project_id WHERE p.uid=$1`, projectUID).Scan(&floor); err != nil {
			return err
		}
		record = TrackerActivation{Version: 1, RootInstanceUID: a.InstanceUID, ProjectUID: projectUID, Connection: *selection, BindingID: bindingID, SourceKey: source, ProcessEpoch: epoch, Active: true, EventFloor: floor}
		return writeTrackerActivationSQL(ctx, tx, record)
	})
	return record, err
}
func writeTrackerActivationSQL(ctx context.Context, tx *sql.Tx, record TrackerActivation) error {
	if record.Validate() != nil {
		return ErrTrackerFenceInvalid
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, TrackerActivationKey(record.RootInstanceUID, record.ProjectUID), string(raw))
	return err
}
func isTrackerActivationKey(key string) bool {
	return strings.HasPrefix(key, TrackerMetadataPrefix+"activation.")
}
func trackerActivationMetadataForImport(record MetaKV, opts ImportOptions) (MetaKV, error) {
	var a TrackerActivation
	if len(record.Value) > 8192 || json.Unmarshal([]byte(record.Value), &a, json.RejectUnknownMembers(true)) != nil || a.Validate() != nil || record.Key != TrackerActivationKey(a.RootInstanceUID, a.ProjectUID) {
		return MetaKV{}, ErrTrackerFenceInvalid
	}
	a.Active = false
	a.RestoredHistoryGap = a.RestoredHistoryGap || !opts.PreserveIssueSyncBindingEnabled
	raw, err := json.Marshal(a)
	if err != nil {
		return MetaKV{}, err
	}
	record.Value = string(raw)
	return record, nil
}

// EnsureTrackerRestoreBarriersSQL runs after full/project replay. A named
// binding gets an inactive record even if its backup contains no operation or
// lifecycle row. Existing (already normalized) retained evidence is preserved.
func EnsureTrackerRestoreBarriersSQL(ctx context.Context, tx *sql.Tx, opts ImportOptions, records []ImportRecord) error {
	var root string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&root); err != nil {
		return err
	}
	if !uid.Valid(root) {
		return ErrTrackerFenceInvalid
	}
	importedProjects := map[string]bool{}
	sourceInstance := ""
	for _, record := range records {
		switch record := record.(type) {
		case *ProjectExport:
			if record.UID != SystemProjectUID {
				importedProjects[record.UID] = true
			}
		case *MetaKV:
			if record.Key == "instance_uid" {
				sourceInstance = record.Value
			}
		}
	}
	if !opts.PreserveIssueSyncBindingEnabled {
		// A purported clone whose retained identity equals the source is still a
		// same-instance restore. An absent source identity cannot prove otherwise.
		global := !opts.MergeProject && (!opts.NewInstance || sourceInstance == root || !uid.Valid(sourceInstance))
		if global {
			if err := writeTrackerRestoreBarrierSQL(ctx, tx, root, ""); err != nil {
				return err
			}
		} else {
			// A portable import can predate its first binding too. Restrict that
			// history gap to imported projects, preserving unrelated target authority.
			for project := range importedProjects {
				if err := writeTrackerRestoreBarrierSQL(ctx, tx, root, project); err != nil {
					return err
				}
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.id,p.uid,b.source_key,b.config_json,b.connection_uid FROM issue_sync_bindings b JOIN projects p ON p.id=b.project_id WHERE b.connection_uid IS NOT NULL`)
	if err != nil {
		return err
	}
	var activations []TrackerActivation
	for rows.Next() {
		var a TrackerActivation
		var config string
		var marker *string
		if err := rows.Scan(&a.BindingID, &a.ProjectUID, &a.SourceKey, &config, &marker); err != nil {
			_ = rows.Close()
			return err
		}
		if opts.MergeProject && !importedProjects[a.ProjectUID] {
			continue
		}
		if err := ValidateIssueSyncConnectionMarker(marker, []byte(config)); err != nil {
			_ = rows.Close()
			return err
		}
		selection, err := IssueSyncConnectionFromConfig([]byte(config))
		if err != nil || selection == nil {
			_ = rows.Close()
			return ErrTrackerFenceInvalid
		}
		a.Version = 1
		a.RestoredHistoryGap = !opts.PreserveIssueSyncBindingEnabled
		a.RootInstanceUID = root
		a.ProcessEpoch = root
		a.Connection = *selection
		activations = append(activations, a)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, a := range activations {
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, TrackerActivationKey(root, a.ProjectUID), string(raw)); err != nil {
			return err
		}
	}
	return nil
}
