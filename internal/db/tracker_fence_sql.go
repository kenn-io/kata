package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// TrackerFenceStore is a narrow optional interface shared by native backends.
// The caller supplies its actual daemon-lifetime epoch, never an HTTP body value.
type TrackerFenceStore interface {
	LoadTrackerExecutionFence(context.Context, string, string, string) (TrackerExecutionFence, bool, error)
	StoreTrackerExecutionFence(context.Context, *TrackerExecutionFence, TrackerExecutionFence, string) error
}

// TrackerFenceSQL uses existing transactional meta storage. Native BeginTx and
// the explicit project check retain request, host and project authorization.
type TrackerFenceSQL struct {
	Postgres    bool // selects the native byte collation for metadata keyset pages
	InstanceUID string
	Transact    func(context.Context, func(*sql.Tx) error) error
}

func TrackerExecutionFenceKey(rootUID, projectUID, role string) (string, error) {
	if !uid.Valid(rootUID) || !uid.Valid(projectUID) || (role != "coordinator" && role != "executor") {
		return "", ErrTrackerFenceInvalid
	}
	return TrackerMetadataPrefix + projectUID + "." + role + "." + rootUID, nil
}

func trackerKeyMatches(key string, f TrackerExecutionFence) bool {
	want, err := TrackerExecutionFenceKey(f.RootInstanceUID, f.ProjectUID, f.Role)
	return err == nil && key == want
}

func trackerHistoryKey(f TrackerExecutionFence) string {
	key, _ := TrackerExecutionFenceKey(f.RootInstanceUID, f.ProjectUID, f.Role)
	return key + ".history." + f.WriteUID
}

func trackerImportKeyMatches(key string, f TrackerExecutionFence) bool {
	return trackerKeyMatches(key, f) || (trackerFenceSettled(f) && key == trackerHistoryKey(f))
}

func (a TrackerFenceSQL) authorize(ctx context.Context, tx *sql.Tx, projectUID string) error {
	if allowed, restricted := AuthorizedProjects(ctx); restricted && !slices.Contains(allowed, projectUID) {
		return ErrNotFound
	}
	if err := CheckProjectAccessTransaction(ctx, tx, projectUID); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE uid=$1 AND deleted_at IS NULL`, projectUID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("check tracker fence project: %w", err)
	}
	return nil
}

func (a TrackerFenceSQL) local(f TrackerExecutionFence) bool {
	return (f.Role == "coordinator" && f.RootInstanceUID == a.InstanceUID) || (f.Role == "executor" && f.Connection.OwnerInstanceUID == a.InstanceUID)
}

func (a TrackerFenceSQL) Load(ctx context.Context, rootUID, projectUID, role string) (TrackerExecutionFence, bool, error) {
	key, err := TrackerExecutionFenceKey(rootUID, projectUID, role)
	if err != nil {
		return TrackerExecutionFence{}, false, err
	}
	var f TrackerExecutionFence
	found := false
	err = a.Transact(ctx, func(tx *sql.Tx) error {
		f, found = TrackerExecutionFence{}, false
		if err := a.authorize(ctx, tx, projectUID); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("read tracker execution fence: %w", err)
		}
		if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &f, json.RejectUnknownMembers(true)) != nil || f.Validate() != nil || !trackerKeyMatches(key, f) || !a.local(f) {
			return ErrTrackerFenceInvalid
		}
		found = true
		return nil
	})
	return f, found, err
}

func (a TrackerFenceSQL) Store(ctx context.Context, previous *TrackerExecutionFence, next TrackerExecutionFence, processEpoch string) error {
	if next.Validate() != nil || !a.local(next) || !uid.Valid(processEpoch) {
		return ErrTrackerFenceInvalid
	}
	key, err := TrackerExecutionFenceKey(next.RootInstanceUID, next.ProjectUID, next.Role)
	if err != nil {
		return err
	}
	if previous == nil {
		if next.Sequence != 1 || !next.CanDispatch(processEpoch) {
			return ErrTrackerOutcomeUnknown
		}
	} else {
		if err := ValidateTrackerFenceTransition(*previous, next); err != nil {
			return err
		}
		if next.Phase == TrackerWriteDispatching && !previous.CanDispatch(processEpoch) {
			return ErrTrackerOutcomeUnknown
		}
		if next.Phase == TrackerWriteNotSent && previous.Phase != TrackerWriteNotSent && !previous.CanDispatch(processEpoch) {
			return ErrTrackerOutcomeUnknown
		}
	}
	if next.Phase == TrackerWritePrepared && !next.CanDispatch(processEpoch) {
		return ErrTrackerOutcomeUnknown
	}
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > 32<<10 {
		return ErrTrackerFenceInvalid
	}
	var expected []byte
	if previous != nil {
		expected, err = json.Marshal(previous)
		if err != nil {
			return ErrTrackerFenceInvalid
		}
	}
	return a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, next.ProjectUID); err != nil {
			return err
		}
		// A new sequence cannot recycle an old semantic event. This history
		// survives the current head, connection changes and stale run claims.
		// A missing head is not proof that the semantic event is new.
		if next.Phase == TrackerWritePrepared && (previous == nil || next.Sequence != previous.Sequence) {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key=$1`, trackerHistoryKey(next)).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return ErrTrackerOutcomeUnknown
			}
		}
		var result sql.Result
		var err error
		if previous == nil {
			result, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, key, string(raw))
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`, string(raw), key, string(expected))
		}
		if err != nil {
			return fmt.Errorf("store tracker execution fence: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrTrackerFenceChanged
		}
		if trackerFenceSettled(next) {
			// Head acknowledgement and retained replay evidence are one commit.
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, trackerHistoryKey(next), string(raw)); err != nil {
				return err
			}
			var retained string
			if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, trackerHistoryKey(next)).Scan(&retained); err != nil {
				return err
			}
			if retained != string(raw) {
				return ErrTrackerFenceChanged
			}
		}
		if next.Role == "coordinator" && next.Phase == TrackerWriteAcknowledged {
			return advanceTrackerOwnerCheckpointSQL(ctx, tx, next)
		}
		return nil
	})
}

// TrackerMetadataForImport retains execution evidence for a full restore, but
// never activates it. Project transfer/new instance cannot import local authority.
func TrackerMetadataForImport(record MetaKV, opts ImportOptions) (MetaKV, bool, error) {
	if !strings.HasPrefix(record.Key, TrackerMetadataPrefix) {
		return record, true, nil
	}
	if opts.MergeProject || opts.NewInstance {
		return MetaKV{}, false, nil
	}
	if isTrackerOwnerCheckpointKey(record.Key) {
		normalized, err := trackerOwnerCheckpointForImport(record, opts)
		return normalized, err == nil, err
	}
	if isTrackerRestoreBarrierKey(record.Key) {
		normalized, err := trackerRestoreBarrierForImport(record)
		return normalized, err == nil, err
	}
	if isTrackerControlKey(record.Key) {
		normalized, err := trackerControlMetadataForImport(record)
		return normalized, err == nil, err
	}
	if isTrackerActivationKey(record.Key) {
		normalized, err := trackerActivationMetadataForImport(record, opts)
		return normalized, err == nil, err
	}
	if isTrackerRegistryKey(record.Key) {
		normalized, err := trackerRegistryMetadataForImport(record)
		return normalized, err == nil, err
	}
	var f TrackerExecutionFence
	if len(record.Value) > 32<<10 || json.Unmarshal([]byte(record.Value), &f, json.RejectUnknownMembers(true)) != nil || f.Validate() != nil || !trackerImportKeyMatches(record.Key, f) {
		return MetaKV{}, false, ErrTrackerFenceInvalid
	}
	f.ActivationRequired = true
	if f.Phase == TrackerWritePrepared || f.Phase == TrackerWriteDispatching {
		f.Phase = TrackerWriteUnknown
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return MetaKV{}, false, ErrTrackerFenceInvalid
	}
	record.Value = string(raw)
	return record, true, nil
}

type TrackerFenceHistoryStore interface {
	LoadTrackerExecutionHistory(context.Context, string, string, string, string) (TrackerExecutionFence, bool, error)
}

func (a TrackerFenceSQL) LoadHistory(ctx context.Context, root, project, role, write string) (TrackerExecutionFence, bool, error) {
	key, err := TrackerExecutionFenceKey(root, project, role)
	if err != nil || !trackerDigest(write) {
		return TrackerExecutionFence{}, false, ErrTrackerFenceInvalid
	}
	var f TrackerExecutionFence
	var found bool
	err = a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, project); err != nil {
			return err
		}
		var err error
		f, found, err = loadTrackerFenceKeySQL(ctx, tx, key+".history."+write)
		if err != nil {
			return err
		}
		if found && (!a.local(f) || !trackerFenceSettled(f)) {
			return ErrTrackerFenceInvalid
		}
		return nil
	})
	return f, found, err
}

// metadataPageQuery uses only trusted backend constants. Prefix range and
// keyset ordering must use the same byte order on every supported DB locale.
func (a TrackerFenceSQL) metadataPageQuery() string {
	order := "key COLLATE BINARY"
	if a.Postgres {
		order = `key COLLATE "C"`
	}
	return `SELECT key,value FROM meta WHERE ` + order + `>$1 AND ` + order + `<$2 ORDER BY ` + order + ` LIMIT $3`
}

type TrackerRecoveryHistoryLister interface {
	ListTrackerRecoveryHistory(context.Context, string, string, string, int) ([]TrackerExecutionFence, error)
}

func (a TrackerFenceSQL) ListRecoveryHistory(ctx context.Context, root, project, after string, limit int) ([]TrackerExecutionFence, error) {
	if root != a.InstanceUID || !uid.Valid(root) || !uid.Valid(project) || (after != "" && !trackerDigest(after)) || limit < 1 || limit > 128 {
		return nil, ErrTrackerFenceInvalid
	}
	key, _ := TrackerExecutionFenceKey(root, project, "coordinator")
	prefix := key + ".history."
	result := []TrackerExecutionFence{}
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		result = result[:0]
		if err := a.authorize(ctx, tx, project); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, a.metadataPageQuery(), prefix+after, prefix+"~", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key, raw string
			if err := rows.Scan(&key, &raw); err != nil {
				return err
			}
			var f TrackerExecutionFence
			if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &f, json.RejectUnknownMembers(true)) != nil || f.Validate() != nil || f.Role != "coordinator" || f.RootInstanceUID != root || f.ProjectUID != project || !trackerFenceSettled(f) || key != trackerHistoryKey(f) {
				return ErrTrackerFenceInvalid
			}
			result = append(result, f)
		}
		return rows.Err()
	})
	return result, err
}
