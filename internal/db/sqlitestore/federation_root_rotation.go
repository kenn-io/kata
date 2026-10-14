package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// RotateRootAuthority applies only a transition signed by the currently pinned
// key. Native policy/binding fences serialize the key change and retained proof.
func (d *Store) RotateRootAuthority(ctx context.Context, transition db.RootKeyTransition) error {
	if transition.Next.Retired {
		return errors.New("replacement root key must be active")
	}
	if err := db.ValidateRootKeyPin(transition.Next); err != nil {
		return err
	}
	if !db.ProjectAttributionVisible(ctx, transition.Next.ProjectUID) {
		return db.ErrNotFound
	}
	raw, err := json.Marshal(transition)
	if err != nil {
		return err
	}
	return d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		var projectID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=? AND deleted_at IS NULL`, transition.Next.ProjectUID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			return err
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, projectID))
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
		if err == nil && binding.RelayConfig != nil {
			if _, err := d.upstreamRelayGrantTx(ctx, tx, binding.RelayConfig.BindingUID); err != nil {
				return err
			}
		}
		current, err := rootPinTx(ctx, tx, transition.Next.ProjectUID, "")
		if err != nil {
			return err
		}
		key := db.RootKeyTransitionMetadataKey(transition)
		if current.KeyID == transition.Next.KeyID {
			var retained string
			if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&retained); err != nil {
				return err
			}
			if retained != string(raw) {
				return errors.New("root transition retry differs from retained authorization")
			}
			previous, err := rootPinTx(ctx, tx, transition.Next.ProjectUID, transition.PreviousKeyID)
			if err != nil {
				return err
			}
			return db.VerifyRootKeyTransition(previous, transition)
		}
		if err := db.VerifyRootKeyTransition(current, transition); err != nil {
			return err
		}
		// Retired keys are historical verification material, never automatic fallback.
		_, err = rootPinTx(ctx, tx, transition.Next.ProjectUID, transition.Next.KeyID)
		if err == nil {
			return errors.New("replacement root key was already retained")
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_root_keys SET active=0 WHERE project_uid=? AND key_id=? AND active=1`, current.ProjectUID, current.KeyID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES(?,?,?,?,1)`, transition.Next.ProjectUID, transition.Next.AuthorityUID, transition.Next.KeyID, base64.StdEncoding.EncodeToString(transition.Next.PublicKey)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?)`, key, string(raw))
		return err
	})
}

// RepinRootAuthority replaces a local root project's active pin without a
// signed transition. The previous key stays retained for historical receipts.
// A stale previous key, another authority, or a retained key is refused, and
// so is stranding active relay enrollments without strandRelays. The
// enrollment count runs under the same project-access fence that enrollment
// creation takes, so a grant issued by another daemon cannot slip between the
// count and the replacement.
func (d *Store) RepinRootAuthority(ctx context.Context, previousKeyID string, next db.RootKeyPin, strandRelays bool) error {
	if next.Retired {
		return errors.New("replacement root key must be active")
	}
	if err := db.ValidateRootKeyPin(next); err != nil {
		return err
	}
	if next.AuthorityUID != d.instanceUID {
		return errors.New("only this daemon's own root projects can be repinned")
	}
	if !db.ProjectAttributionVisible(ctx, next.ProjectUID) {
		return db.ErrNotFound
	}
	return d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		var projectID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=? AND deleted_at IS NULL`, next.ProjectUID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			return err
		}
		current, err := rootPinTx(ctx, tx, next.ProjectUID, "")
		if err != nil {
			return err
		}
		if current.AuthorityUID != d.instanceUID {
			return errors.New("only this daemon's own root projects can be repinned")
		}
		if current.KeyID == next.KeyID {
			return nil
		}
		if current.KeyID != previousKeyID {
			return errors.New("root pin changed since repin preflight; review it and retry")
		}
		if !strandRelays {
			var enrollments int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_enrollments WHERE revoked_at IS NULL AND (project_id=? OR project_id IS NULL)`, projectID).Scan(&enrollments); err != nil {
				return err
			}
			if enrollments > 0 {
				return &db.RootRepinStrandsRelaysError{Enrollments: enrollments}
			}
		}
		// Retired keys are historical verification material, never a fallback.
		_, err = rootPinTx(ctx, tx, next.ProjectUID, next.KeyID)
		if err == nil {
			return errors.New("replacement root key was already retained for this project; use a new key")
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_root_keys SET active=0 WHERE project_uid=? AND key_id=? AND active=1`, current.ProjectUID, current.KeyID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES(?,?,?,?,1)`, next.ProjectUID, next.AuthorityUID, next.KeyID, base64.StdEncoding.EncodeToString(next.PublicKey))
		return err
	})
}

// RootKeyTransitions reads a project’s retained public root-key transition history.
func (d *Store) RootKeyTransitions(ctx context.Context, projectUID string) ([]db.RootKeyTransition, error) {
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return nil, db.ErrNotFound
	}
	if _, err := d.RootAuthority(ctx, projectUID); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT value FROM meta WHERE key LIKE ? ORDER BY key`, db.RootKeyTransitionMetadataPrefix+projectUID+".%")
	if err != nil {
		return nil, err
	}
	var output []db.RootKeyTransition
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var transition db.RootKeyTransition
		if err = json.Unmarshal([]byte(raw), &transition, json.RejectUnknownMembers(true)); err != nil {
			break
		}
		output = append(output, transition)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, transition := range output {
		if transition.Next.ProjectUID != projectUID || transition.Next.Retired {
			return nil, errors.New("root transition history differs from project")
		}
		previous, err := rootPinTx(ctx, d, projectUID, transition.PreviousKeyID)
		if err != nil {
			return nil, err
		}
		if err := db.VerifyRootKeyTransition(previous, transition); err != nil {
			return nil, err
		}
		if _, err := rootPinTx(ctx, d, projectUID, transition.Next.KeyID); err != nil {
			return nil, err
		}
	}
	return output, nil
}
