package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"

	"go.kenn.io/kata/internal/db"
)

// SetRelayBindingConfig installs bounded, pinned topology on a single existing
// spoke binding. An optional expected configuration makes recovery conditional
// on the exact retained state. Policy/binding locks also fence descendants.
func (d *Store) SetRelayBindingConfig(ctx context.Context, projectID int64, config db.RelayBindingConfig, expected ...db.RelayBindingConfig) (db.FederationBinding, error) {
	if err := config.Validate(d.instanceUID); err != nil {
		return db.FederationBinding{}, err
	}
	var output db.FederationBinding
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		if err := lockProjectAccess(ctx, tx); err != nil {
			return err
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=? `, projectID))
		if err != nil {
			return err
		}
		// Sync recovery must not overwrite an operator change made during
		// the authorized handshake. Compare under the existing binding lock.
		if len(expected) > 1 || (len(expected) == 1 && !reflect.DeepEqual(binding.RelayConfig, &expected[0])) {
			return db.ErrRemoteEventConflict
		}
		if binding.Role != db.FederationRoleSpoke || !binding.Enabled || !binding.PushEnabled || binding.Actor == "" {
			return db.ErrNotFound
		}
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL `, projectID).Scan(&projectUID); err != nil {
			return db.ErrNotFound
		}
		if projectUID != binding.HubProjectUID {
			return errors.New("relay requires the pinned project UID")
		}
		if err := d.ProjectAccessTransactionFence(config.LocalActor, []string{projectUID})(ctx, tx); err != nil {
			return err
		}
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			return db.ErrNotFound
		}
		pin, err := rootPinTx(ctx, tx, projectUID, "")
		if err != nil {
			return err
		}
		if err := rejectRelayProjectLinksTx(ctx, tx, projectID); err != nil {
			return err
		}
		if pin.AuthorityUID != config.AuthorityUID {
			return errors.New("relay authority differs from pinned root")
		}
		if prior := binding.RelayConfig; prior != nil {
			if prior.BindingUID != config.BindingUID || prior.UpstreamInstanceUID != config.UpstreamInstanceUID || prior.AuthorityUID != config.AuthorityUID || prior.LocalActor != config.LocalActor || prior.ResetEpoch != config.ResetEpoch || !slices.Equal(prior.HubPath, config.HubPath) {
				return errors.New("relay peer, path and epoch require an authenticated reset")
			}
		}
		// A hop UID may not alias another project's binding or a downstream grant.
		var aliases int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_enrollments WHERE relay_binding_uid=?`, config.BindingUID).Scan(&aliases); err != nil {
			return err
		}
		if aliases != 0 {
			return errors.New("relay binding UID already identifies a downstream grant")
		}
		rows, err := tx.QueryContext(ctx, `SELECT relay_config FROM federation_bindings WHERE project_id<>? AND relay_config IS NOT NULL`, projectID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw *string
			if err = rows.Scan(&raw); err != nil {
				break
			}
			other, e := db.DecodeRelayBindingConfig(raw)
			if e != nil {
				err = e
				break
			}
			if other.BindingUID == config.BindingUID {
				err = errors.New("relay binding UID already identifies another project")
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET relay_config=? WHERE project_id=?`, string(raw), projectID); err != nil {
			return err
		}
		grant := db.FederationEnrollment{ProjectID: &projectID, RelayBindingUID: config.BindingUID, RelayProtocolVersion: config.ProtocolVersion, RelayResetEpoch: config.ResetEpoch, SpokeInstanceUID: config.UpstreamInstanceUID}
		if err := d.seedRelayArtifactManifestsTx(ctx, tx, grant, projectUID); err != nil {
			return err
		}
		if err := d.seedRelayLocalEventsTx(ctx, tx, grant, projectUID, binding.PushCursorEventID); err != nil {
			return err
		}
		output, err = scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, projectID))
		return err
	})
	return output, err
}

// relayServingBindingTx keeps legacy hub admission unchanged. Only negotiated,
// current relay spokes may also serve the narrowed descendants.
func (d *Store) relayServingBindingTx(ctx context.Context, tx db.Transaction, projectID int64, protocol int) (db.FederationBinding, string, error) {
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=? `, projectID))
	if err != nil {
		return binding, "", err
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL `, projectID).Scan(&projectUID); err != nil {
		return binding, "", db.ErrNotFound
	}
	if !binding.Enabled {
		return binding, "", db.ErrNotFound
	}
	if binding.Role == db.FederationRoleHub {
		return binding, projectUID, nil
	}
	config := binding.RelayConfig
	if protocol != db.RelayProtocolVersion || binding.Role != db.FederationRoleSpoke || !binding.PushEnabled || config == nil || !config.ServeDownstream || config.UpstreamRevoked || binding.HubProjectUID != projectUID {
		return binding, "", db.ErrNotFound
	}
	if err := config.Validate(d.instanceUID); err != nil {
		return binding, "", err
	}
	pin, err := rootPinTx(ctx, tx, projectUID, "")
	if err != nil {
		return binding, "", err
	}
	if pin.AuthorityUID != config.AuthorityUID {
		return binding, "", db.ErrNotFound
	}
	if err := d.ProjectAccessTransactionFence(config.LocalActor, []string{projectUID})(ctx, tx); err != nil {
		return binding, "", err
	}
	return binding, projectUID, nil
}

// upstreamRelayGrantTx is internal admission for the locally configured outgoing
// binding. It does not accept an account, project or source label from the wire.
func (d *Store) upstreamRelayGrantTx(ctx context.Context, tx *sql.Tx, bindingUID string) (db.FederationEnrollment, error) {
	rows, err := tx.QueryContext(ctx, federationBindingSelect+` WHERE role='spoke' AND relay_config IS NOT NULL ORDER BY project_id`)
	if err != nil {
		return db.FederationEnrollment{}, err
	}
	var candidates []db.FederationBinding
	for rows.Next() {
		var b db.FederationBinding
		b, err = scanFederationBinding(rows)
		if err != nil {
			break
		}
		if b.RelayConfig.BindingUID == bindingUID {
			candidates = append(candidates, b)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return db.FederationEnrollment{}, err
	}
	if len(candidates) != 1 {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	b := candidates[0]
	// Leaf clients need upstream delivery even when they do not serve descendants.
	b, err = scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=? `, b.ProjectID))
	if err != nil {
		return db.FederationEnrollment{}, err
	}
	c := b.RelayConfig
	if c == nil || !b.Enabled || !b.PushEnabled || c.UpstreamRevoked || c.BindingUID != bindingUID {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	if err := c.Validate(d.instanceUID); err != nil {
		return db.FederationEnrollment{}, err
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL `, b.ProjectID).Scan(&projectUID); err != nil {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	if projectUID != b.HubProjectUID {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	pin, err := rootPinTx(ctx, tx, projectUID, "")
	if err != nil {
		return db.FederationEnrollment{}, err
	}
	if pin.AuthorityUID != c.AuthorityUID {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	if err := d.ProjectAccessTransactionFence(c.LocalActor, []string{projectUID})(ctx, tx); err != nil {
		return db.FederationEnrollment{}, err
	}
	return db.FederationEnrollment{ProjectID: &b.ProjectID, RelayBindingUID: c.BindingUID, RelayProtocolVersion: c.ProtocolVersion, RelayResetEpoch: c.ResetEpoch, SpokeInstanceUID: c.UpstreamInstanceUID, Actor: c.LocalActor}, nil
}
