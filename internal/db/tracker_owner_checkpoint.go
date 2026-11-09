package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"math"
	"strings"

	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

// TrackerRemoteAttempt binds independently sequenced owner and coordinator
// lanes to one exact root-issued reservation. It is retained evidence, never an
// executable ticket. Owner generation changes do not create a new owner lane.
type TrackerRemoteAttempt struct {
	UID               string `json:"uid"`
	ArgumentsDigest   string `json:"arguments_digest"`
	RootSequence      uint64 `json:"root_sequence"`
	RootPreviousHead  string `json:"root_previous_head,omitempty"`
	OwnerSequence     uint64 `json:"owner_sequence"`
	OwnerPreviousHead string `json:"owner_previous_head,omitempty"`
	RootEpoch         string `json:"root_epoch"`
	OwnerEpoch        string `json:"owner_epoch"`
}

func trackerSequenceValid(sequence uint64, previous string) bool {
	return sequence > 0 && sequence <= math.MaxInt64 && ((sequence == 1 && previous == "") || (sequence > 1 && tracker.DigestValid(previous)))
}
func (r TrackerRemoteAttempt) Validate() error {
	if !uid.Valid(r.UID) || !tracker.DigestValid(r.ArgumentsDigest) || !trackerSequenceValid(r.RootSequence, r.RootPreviousHead) || !trackerSequenceValid(r.OwnerSequence, r.OwnerPreviousHead) || !uid.Valid(r.RootEpoch) || !uid.Valid(r.OwnerEpoch) {
		return ErrTrackerFenceInvalid
	}
	return nil
}
func TrackerExecutorPreparation(root TrackerExecutionFence, epoch string) (TrackerExecutionFence, error) {
	if root.Validate() != nil || root.Role != "coordinator" || root.Remote == nil || root.Phase != TrackerWritePrepared || !uid.Valid(epoch) || root.ActivationRequired {
		return TrackerExecutionFence{}, ErrTrackerFenceInvalid
	}
	owner := root
	owner.Role = "executor"
	owner.Sequence = root.Remote.OwnerSequence
	owner.PreviousHead = root.Remote.OwnerPreviousHead
	owner.ProcessEpoch = epoch
	return owner, owner.Validate()
}

// Checkpoints contain the exact acknowledged/non-sent owner head, not a maximum
// sequence imported from a peer. Native CAS compares the previous checkpoint.
type TrackerOwnerCheckpoint struct {
	Version          int                  `json:"version"`
	RootInstanceUID  string               `json:"root_instance_uid"`
	ProjectUID       string               `json:"project_uid"`
	OwnerInstanceUID string               `json:"owner_instance_uid"`
	Selection        tracker.Selection    `json:"selection"`
	Sequence         uint64               `json:"sequence"`
	Head             string               `json:"head"`
	Attempt          TrackerRemoteAttempt `json:"attempt"`
	WriteUID         string               `json:"write_uid"`
	Disposition      TrackerWritePhase    `json:"disposition"`
	ResultDigest     string               `json:"result_digest"`
	Restored         bool                 `json:"restored"`
}

func TrackerOwnerCheckpointKey(root, project, owner string) string {
	return TrackerMetadataPrefix + "checkpoint." + root + "." + project + "." + owner
}
func (c TrackerOwnerCheckpoint) Validate() error {
	if c.Version != 1 || !uid.Valid(c.RootInstanceUID) || !uid.Valid(c.ProjectUID) || !uid.Valid(c.OwnerInstanceUID) || c.Selection.Validate() != nil || c.Selection.OwnerInstanceUID != c.OwnerInstanceUID || c.Attempt.Validate() != nil || c.Sequence != c.Attempt.OwnerSequence || !tracker.DigestValid(c.Head) || !tracker.DigestValid(c.WriteUID) || !tracker.DigestValid(c.ResultDigest) || (c.Disposition != TrackerWriteAcknowledged && c.Disposition != TrackerWriteNotSent) {
		return ErrTrackerFenceInvalid
	}
	return nil
}
func isTrackerOwnerCheckpointKey(key string) bool {
	return strings.HasPrefix(key, TrackerMetadataPrefix+"checkpoint.")
}
func trackerOwnerCheckpointForImport(record MetaKV, opts ImportOptions) (MetaKV, error) {
	var c TrackerOwnerCheckpoint
	if len(record.Value) > 8192 || json.Unmarshal([]byte(record.Value), &c, json.RejectUnknownMembers(true)) != nil || c.Validate() != nil || record.Key != TrackerOwnerCheckpointKey(c.RootInstanceUID, c.ProjectUID, c.OwnerInstanceUID) {
		return MetaKV{}, ErrTrackerFenceInvalid
	}
	c.Restored = c.Restored || !opts.PreserveIssueSyncBindingEnabled
	raw, err := json.Marshal(c)
	record.Value = string(raw)
	return record, err
}
func loadTrackerOwnerCheckpointSQL(ctx context.Context, tx *sql.Tx, root, project, owner string) (TrackerOwnerCheckpoint, bool, error) {
	var c TrackerOwnerCheckpoint
	var raw string
	key := TrackerOwnerCheckpointKey(root, project, owner)
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if len(raw) > 8192 || json.Unmarshal([]byte(raw), &c, json.RejectUnknownMembers(true)) != nil || c.Validate() != nil || key != TrackerOwnerCheckpointKey(c.RootInstanceUID, c.ProjectUID, c.OwnerInstanceUID) {
		return c, false, ErrTrackerFenceInvalid
	}
	return c, true, nil
}
func (a TrackerFenceSQL) LoadOwnerCheckpoint(ctx context.Context, root, project, owner string) (TrackerOwnerCheckpoint, bool, error) {
	var c TrackerOwnerCheckpoint
	var found bool
	if !uid.Valid(root) || !uid.Valid(project) || !uid.Valid(owner) || root != a.InstanceUID {
		return c, false, ErrTrackerFenceInvalid
	}
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, project); err != nil {
			return err
		}
		var err error
		c, found, err = loadTrackerOwnerCheckpointSQL(ctx, tx, root, project, owner)
		return err
	})
	return c, found, err
}
func validateTrackerOwnerPredecessorSQL(ctx context.Context, tx *sql.Tx, root TrackerExecutionFence) error {
	if root.Remote == nil {
		return nil
	}
	c, found, err := loadTrackerOwnerCheckpointSQL(ctx, tx, root.RootInstanceUID, root.ProjectUID, root.Connection.OwnerInstanceUID)
	if err != nil {
		return err
	}
	if !found {
		if root.Remote.OwnerSequence != 1 || root.Remote.OwnerPreviousHead != "" {
			return ErrTrackerFenceChanged
		}
		return nil
	}
	if c.Restored || c.Sequence+1 != root.Remote.OwnerSequence || c.Head != root.Remote.OwnerPreviousHead {
		return ErrTrackerOutcomeUnknown
	}
	return nil
}

// advanceTrackerOwnerCheckpointSQL is called from the SAME transaction as
// root exact-event acknowledgement or proven non-send settlement. It cannot
// acknowledge a provider, mutate an issue or clear restoration uncertainty.
func advanceTrackerOwnerCheckpointSQL(ctx context.Context, tx *sql.Tx, root TrackerExecutionFence) error {
	if root.Remote == nil {
		return nil
	}
	if root.Role != "coordinator" || !trackerFenceSettled(root) {
		return ErrTrackerFenceInvalid
	}
	owner, err := TrackerExecutorPreparationForCheckpoint(root)
	if err != nil {
		return err
	}
	next := TrackerOwnerCheckpoint{Version: 1, RootInstanceUID: root.RootInstanceUID, ProjectUID: root.ProjectUID, OwnerInstanceUID: root.Connection.OwnerInstanceUID, Selection: root.Connection, Sequence: root.Remote.OwnerSequence, Head: TrackerFenceHead(owner), Attempt: *root.Remote, WriteUID: root.WriteUID, Disposition: root.Phase, ResultDigest: tracker.WorkerArgumentsDigest(root.Result)}
	if err := next.Validate(); err != nil {
		return err
	}
	current, found, err := loadTrackerOwnerCheckpointSQL(ctx, tx, next.RootInstanceUID, next.ProjectUID, next.OwnerInstanceUID)
	if err != nil {
		return err
	}
	key := TrackerOwnerCheckpointKey(next.RootInstanceUID, next.ProjectUID, next.OwnerInstanceUID)
	if found {
		next.Restored = current.Restored
	}
	if blocked, err := trackerRestoreBarrierPresentSQL(ctx, tx, root.RootInstanceUID, root.ProjectUID); err != nil {
		return err
	} else if blocked {
		next.Restored = true
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if found && current == next {
		return nil
	}
	if (!found && (root.Remote.OwnerSequence != 1 || root.Remote.OwnerPreviousHead != "")) || (found && (current.Sequence+1 != root.Remote.OwnerSequence || current.Head != root.Remote.OwnerPreviousHead)) {
		return ErrTrackerFenceChanged
	}
	var result sql.Result
	if found {
		expected, err := json.Marshal(current)
		if err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`, string(raw), key, string(expected))
		if err != nil {
			return err
		}
	} else {
		result, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, key, string(raw))
		if err != nil {
			return err
		}
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrTrackerFenceChanged
	}
	return nil
}
func TrackerExecutorPreparationForCheckpoint(root TrackerExecutionFence) (TrackerExecutionFence, error) {
	if root.Validate() != nil || root.Remote == nil {
		return TrackerExecutionFence{}, ErrTrackerFenceInvalid
	}
	owner := root
	owner.Role = "executor"
	owner.Sequence = root.Remote.OwnerSequence
	owner.PreviousHead = root.Remote.OwnerPreviousHead
	return owner, owner.Validate()
}

type TrackerOwnerCheckpointStore interface {
	LoadTrackerOwnerCheckpoint(context.Context, string, string, string) (TrackerOwnerCheckpoint, bool, error)
}

// TrackerOwnerCheckpointLister pages native retained identities without holding
// a transaction across owner RPCs. It grants no provider or execution authority.
type TrackerOwnerCheckpointLister interface {
	ListTrackerOwnerCheckpoints(context.Context, string, string, string, int) ([]TrackerOwnerCheckpoint, error)
}

func (a TrackerFenceSQL) ListOwnerCheckpoints(ctx context.Context, root, project, after string, limit int) ([]TrackerOwnerCheckpoint, error) {
	if root != a.InstanceUID || !uid.Valid(root) || !uid.Valid(project) || (after != "" && !uid.Valid(after)) || limit < 1 || limit > 128 {
		return nil, ErrTrackerFenceInvalid
	}
	result := []TrackerOwnerCheckpoint{}
	prefix := TrackerMetadataPrefix + "checkpoint." + root + "." + project + "."
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		result = result[:0]
		if err := a.authorize(ctx, tx, project); err != nil {
			return err
		}
		// Range bounds use only validated ULIDs and the fixed namespace. No LIKE
		// wildcard can accidentally include another project or metadata kind.
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
			var cp TrackerOwnerCheckpoint
			if len(raw) > 8192 || json.Unmarshal([]byte(raw), &cp, json.RejectUnknownMembers(true)) != nil || cp.Validate() != nil || cp.RootInstanceUID != root || cp.ProjectUID != project || key != TrackerOwnerCheckpointKey(root, project, cp.OwnerInstanceUID) {
				return ErrTrackerFenceInvalid
			}
			result = append(result, cp)
		}
		return rows.Err()
	})
	return result, err
}
