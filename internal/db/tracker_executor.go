package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"reflect"

	"go.kenn.io/kata/internal/tracker"
)

// TrackerExecutorStore operations write owner-local metadata only. They never bypass spoke
// guards to create bindings, apply issue events or move import cursors.
type TrackerExecutorStore interface {
	PrepareTrackerExecutor(context.Context, TrackerExecutionFence, string, string) (TrackerExecutionFence, error)
	AdmitTrackerExecutor(context.Context, TrackerExecutionFence, string, string) (TrackerExecutionFence, error)
	AcknowledgeTrackerExecutor(context.Context, TrackerOwnerCheckpoint) error
}

// PrepareExecutor records an owner-local reservation without dispatching it.
func (a TrackerFenceSQL) PrepareExecutor(ctx context.Context, reservation TrackerExecutionFence, provider, epoch string) (TrackerExecutionFence, error) {
	prepared, err := TrackerExecutorPreparation(reservation, epoch)
	if err != nil || !a.local(prepared) {
		return TrackerExecutionFence{}, ErrTrackerFenceInvalid
	}
	err = a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, prepared.ProjectUID); err != nil {
			return err
		}
		if blocked, err := trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, prepared.ProjectUID); err != nil {
			return err
		} else if blocked {
			return ErrTrackerOutcomeUnknown
		}
		if err := a.ValidateTrackerRegistryInTransaction(ctx, tx, prepared.Connection, provider, epoch); err != nil {
			return err
		}
		previous, found, err := loadTrackerHeadSQL(ctx, tx, prepared.RootInstanceUID, prepared.ProjectUID, "executor")
		if err != nil {
			return err
		}
		if found {
			expected, _ := json.Marshal(prepared)
			actual, _ := json.Marshal(previous)
			if string(expected) == string(actual) {
				return nil
			}
			return a.StoreInTransaction(ctx, tx, &previous, prepared, epoch)
		}
		return a.StoreInTransaction(ctx, tx, nil, prepared, epoch)
	})
	return prepared, err
}

// AdmitExecutor atomically admits the exact prepared owner attempt.
func (a TrackerFenceSQL) AdmitExecutor(ctx context.Context, prepared TrackerExecutionFence, provider, epoch string) (TrackerExecutionFence, error) {
	dispatched := prepared
	dispatched.Phase = TrackerWriteDispatching
	if prepared.Remote == nil || prepared.Role != "executor" || !prepared.CanDispatch(epoch) || !a.local(prepared) {
		return TrackerExecutionFence{}, ErrTrackerOutcomeUnknown
	}
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, prepared.ProjectUID); err != nil {
			return err
		}
		if blocked, err := trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, prepared.ProjectUID); err != nil {
			return err
		} else if blocked {
			return ErrTrackerOutcomeUnknown
		}
		if err := a.ValidateTrackerRegistryInTransaction(ctx, tx, prepared.Connection, provider, epoch); err != nil {
			return err
		}
		return a.StoreInTransaction(ctx, tx, &prepared, dispatched, epoch)
	})
	return dispatched, err
}

// AcknowledgeExecutor settles an exact retained owner result.
func (a TrackerFenceSQL) AcknowledgeExecutor(ctx context.Context, checkpoint TrackerOwnerCheckpoint) error {
	if checkpoint.Validate() != nil || checkpoint.OwnerInstanceUID != a.InstanceUID {
		return ErrTrackerFenceInvalid
	}
	return a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, checkpoint.ProjectUID); err != nil {
			return err
		}
		owner, found, err := loadTrackerHeadSQL(ctx, tx, checkpoint.RootInstanceUID, checkpoint.ProjectUID, "executor")
		if err != nil {
			return err
		}
		if !found || owner.Remote == nil || *owner.Remote != checkpoint.Attempt || owner.WriteUID != checkpoint.WriteUID || owner.Connection != checkpoint.Selection || TrackerFenceHead(owner) != checkpoint.Head || tracker.WorkerArgumentsDigest(owner.Result) != checkpoint.ResultDigest {
			return ErrTrackerFenceChanged
		}
		if checkpoint.Disposition == TrackerWriteNotSent {
			if owner.Phase != TrackerWriteNotSent {
				return ErrTrackerOutcomeUnknown
			}
			return nil
		}
		if owner.Phase == TrackerWriteAcknowledged {
			return nil
		}
		if owner.Phase != TrackerWriteConfirmed {
			return ErrTrackerOutcomeUnknown
		}
		next := owner
		next.Phase = TrackerWriteAcknowledged
		return a.StoreInTransaction(ctx, tx, &owner, next, owner.ProcessEpoch)
	})
}

// TrackerExecutorTransportFence binds final provider admission to the same live
// replica route and retained root key verified by the worker. It changes no
// stored configuration and grants no provider authority on its own.
func TrackerExecutorTransportFence(instance, project string, expected FederationBinding, pin RootKeyPin) TransactionFence {
	return func(ctx context.Context, tx Transaction) error {
		if expected.Role != FederationRoleSpoke || !expected.Enabled || expected.RelayConfig == nil || expected.RelayConfig.Validate(instance) != nil || expected.RelayConfig.UpstreamRevoked || expected.RelayConfig.AuthorityUID != pin.AuthorityUID || pin.ProjectUID != project {
			return ErrTrackerFenceChanged
		}
		var role, hub string
		var enabled int
		var hubProject int64
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT f.role,f.enabled,f.hub_url,f.hub_project_id,f.relay_config FROM federation_bindings f JOIN projects p ON p.id=f.project_id WHERE p.uid=$1 AND p.deleted_at IS NULL`, project).Scan(&role, &enabled, &hub, &hubProject, &raw); err != nil {
			return ErrTrackerFenceChanged
		}
		relay, err := DecodeRelayBindingConfig(func() *string {
			if !raw.Valid {
				return nil
			}
			return &raw.String
		}())
		if err != nil || relay == nil || role != string(FederationRoleSpoke) || enabled != 1 || hub != expected.HubURL || hubProject != expected.HubProjectID || !reflect.DeepEqual(relay, expected.RelayConfig) {
			return ErrTrackerFenceChanged
		}
		var authority, key, encodedPublic string
		if err := tx.QueryRowContext(ctx, `SELECT authority_uid,key_id,public_key FROM federation_root_keys WHERE project_uid=$1 AND active=1`, project).Scan(&authority, &key, &encodedPublic); err != nil {
			return ErrTrackerFenceChanged
		}
		public, err := base64.StdEncoding.DecodeString(encodedPublic)
		if err != nil || authority != pin.AuthorityUID || key != pin.KeyID || !bytes.Equal(public, pin.PublicKey) {
			return ErrTrackerFenceChanged
		}
		return nil
	}
}
