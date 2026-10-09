package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// CreateRelayEnrollment derives the account from the retained parent token.
// Replay runs the same current-authority checks before returning the same grant.
func (d *Store) CreateRelayEnrollment(ctx context.Context, p db.CreateRelayEnrollmentParams) (db.CreatedFederationEnrollment, error) {
	if p.ProtocolVersion != db.RelayProtocolVersion || p.ProjectID <= 0 || p.ParentTokenID <= 0 || !uid.Valid(p.SpokeInstanceUID) || p.SpokeInstanceUID == d.instanceUID {
		return db.CreatedFederationEnrollment{}, errors.New("invalid relay project, parent, protocol or peer")
	}
	explicit := p.Token != ""
	if !explicit {
		var err error
		p.Token, err = db.NewFederationToken()
		if err != nil {
			return db.CreatedFederationEnrollment{}, err
		}
	}
	bindingUID, err := uid.New()
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	var result db.CreatedFederationEnrollment
	err = d.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.CreatedFederationEnrollment{}
		if err := lockProjectAccess(ctx, tx); err != nil {
			return err
		}
		_, projectUID, err := d.relayServingBindingTx(ctx, tx, p.ProjectID, p.ProtocolVersion)
		if err != nil {
			return err
		}
		parent, err := scanAPIToken(tx.QueryRowContext(ctx, apiTokenSelect+` WHERE id=? `, p.ParentTokenID))
		if err != nil {
			return err
		}
		if parent.Scope != nil || parent.RevokedAt != nil || (parent.ExpiresAt != nil && !time.Now().Before(*parent.ExpiresAt)) || (p.Actor != "" && p.Actor != parent.Actor) {
			return db.ErrNotFound
		}
		if err := d.ProjectAccessTransactionFence(parent.Actor, []string{projectUID})(ctx, tx); err != nil {
			return err
		}
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			return db.ErrNotFound
		}
		pin, err := rootPinTx(ctx, tx, projectUID, "")
		if err != nil {
			return err
		}
		if err := rejectRelayProjectLinksTx(ctx, tx, p.ProjectID); err != nil {
			return err
		}
		binding, _, err := d.relayServingBindingTx(ctx, tx, p.ProjectID, p.ProtocolVersion)
		if err != nil {
			return err
		}
		ancestor := pin.AuthorityUID == p.SpokeInstanceUID
		if binding.RelayConfig != nil {
			if p.ServeDownstream && len(binding.RelayConfig.HubPath) >= db.MaxRelayHubs {
				return fmt.Errorf("%w: relay would exceed the hub limit", db.ErrFederationIngestValidation)
			}
			for _, node := range binding.RelayConfig.HubPath {
				ancestor = ancestor || node == p.SpokeInstanceUID
			}
		}
		if ancestor {
			return errors.New("relay peer would form an authority cycle")
		}
		existing, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE token_hash=?`, db.FederationTokenHash(p.Token)))
		if err == nil {
			if !explicit || existing.RelayProtocolVersion != p.ProtocolVersion || existing.ProjectID == nil || *existing.ProjectID != p.ProjectID || existing.ParentTokenID == nil || (!p.RebindParent && *existing.ParentTokenID != p.ParentTokenID) || existing.Actor != parent.Actor || existing.SpokeInstanceUID != p.SpokeInstanceUID || existing.RevokedAt != nil || existing.RelayServeDownstream != p.ServeDownstream {
				return db.ErrFederationEnrollmentTokenConflict
			}
			// Explicit same-account rebind changes only the issuing credential.
			// The exact transport token, namespace, epochs and cursors remain intact.
			if *existing.ParentTokenID != p.ParentTokenID {
				if _, err := tx.ExecContext(ctx, `UPDATE federation_enrollments SET parent_token_id=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, p.ParentTokenID, existing.ID); err != nil {
					return err
				}
				existing, err = scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE token_hash=?`, db.FederationTokenHash(p.Token)))
				if err != nil {
					return err
				}
			}
			result = db.CreatedFederationEnrollment{Enrollment: existing, Token: p.Token}
			return nil
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO federation_enrollments(token_hash,spoke_instance_uid,project_id,capabilities,bound_actor,relay_binding_uid,relay_protocol_version,parent_token_id,relay_reset_epoch,relay_serve_downstream) VALUES(?,?,?,'claim,pull,push',?,?,1,?,1,?)`, db.FederationTokenHash(p.Token), p.SpokeInstanceUID, p.ProjectID, parent.Actor, bindingUID, p.ParentTokenID, p.ServeDownstream); err != nil {
			return err
		}
		enrollment, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE token_hash=?`, db.FederationTokenHash(p.Token)))
		if err != nil {
			return err
		}
		if err := d.seedRelayEnrollmentTx(ctx, tx, enrollment, projectUID); err != nil {
			return err
		}
		result = db.CreatedFederationEnrollment{Enrollment: enrollment, Token: p.Token}
		return nil
	})
	return result, err
}

func (d *Store) checkRelayParentTx(ctx context.Context, tx db.Transaction, e db.FederationEnrollment, projectID int64) error {
	if e.RelayProtocolVersion == 0 {
		return nil
	}
	if e.RelayProtocolVersion != db.RelayProtocolVersion || e.ParentTokenID == nil || e.ProjectID == nil || *e.ProjectID != projectID || e.RelayBindingUID == "" || e.RelayResetEpoch <= 0 {
		return db.ErrNotFound
	}
	parent, err := scanAPIToken(tx.QueryRowContext(ctx, apiTokenSelect+` WHERE id=? `, *e.ParentTokenID))
	if err != nil {
		return err
	}
	if parent.Scope != nil || parent.Actor != e.Actor || parent.RevokedAt != nil || (parent.ExpiresAt != nil && !time.Now().Before(*parent.ExpiresAt)) {
		return db.ErrNotFound
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL`, projectID).Scan(&projectUID); err != nil {
		return db.ErrNotFound
	}
	return d.ProjectAccessTransactionFence(parent.Actor, []string{projectUID})(ctx, tx)
}

func (d *Store) relayReadAuthority(ctx context.Context, e db.FederationEnrollment, projectID int64) error {
	return d.relayTx(ctx, func(tx *sql.Tx) error { return d.FederationEnrollmentTransactionFence(e, projectID, "pull")(ctx, tx) })
}

func (d *Store) relayTx(ctx context.Context, op func(*sql.Tx) error) error {
	return d.RetryTransient(ctx, func() error {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := op(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}
