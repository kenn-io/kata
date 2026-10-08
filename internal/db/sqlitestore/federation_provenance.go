package sqlitestore

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

func scanRootPin(row rowScanner) (db.RootKeyPin, error) {
	var pin db.RootKeyPin
	var encoded string
	var active int
	if err := row.Scan(&pin.ProjectUID, &pin.AuthorityUID, &pin.KeyID, &encoded, &active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pin, db.ErrNotFound
		}
		return pin, err
	}
	public, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return pin, err
	}
	pin.PublicKey = public
	pin.Retired = active == 0
	return pin, db.ValidateRootKeyPin(pin)
}

func rootPinTx(ctx context.Context, tx db.Transaction, projectUID, keyID string) (db.RootKeyPin, error) {
	if keyID == "" {
		return scanRootPin(tx.QueryRowContext(ctx, `SELECT project_uid,authority_uid,key_id,public_key,active FROM federation_root_keys WHERE project_uid=? AND active=1`, projectUID))
	}
	return scanRootPin(tx.QueryRowContext(ctx, `SELECT project_uid,authority_uid,key_id,public_key,active FROM federation_root_keys WHERE project_uid=? AND key_id=?`, projectUID, keyID))
}

// PinRootAuthority retains the enrolled root identity and public key for a project.
func (d *Store) PinRootAuthority(ctx context.Context, pin db.RootKeyPin) error {
	if pin.Retired {
		return errors.New("cannot enroll a retired root key")
	}
	if err := db.ValidateRootKeyPin(pin); err != nil {
		return err
	}
	if !db.ProjectAttributionVisible(ctx, pin.ProjectUID) {
		return db.ErrNotFound
	}
	return d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		var projectID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=? AND deleted_at IS NULL`, pin.ProjectUID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			return err
		}
		current, err := rootPinTx(ctx, tx, pin.ProjectUID, "")
		if err == nil {
			if current.AuthorityUID == pin.AuthorityUID && current.KeyID == pin.KeyID {
				return nil
			}
			return errors.New("root authority already pinned; signed rotation or explicit repinning required")
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES(?,?,?,?,1)`, pin.ProjectUID, pin.AuthorityUID, pin.KeyID, base64.StdEncoding.EncodeToString(pin.PublicKey))
		return err
	})
}

// RootAuthority reads the active public root pin for a project.
func (d *Store) RootAuthority(ctx context.Context, projectUID string) (db.RootKeyPin, error) {
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return db.RootKeyPin{}, db.ErrNotFound
	}
	return scanRootPin(d.QueryRowContext(ctx, `SELECT project_uid,authority_uid,key_id,public_key,active FROM federation_root_keys WHERE project_uid=? AND active=1`, projectUID))
}

func readReceipt(row rowScanner) (db.AttributionReceipt, error) {
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.AttributionReceipt{}, db.ErrNotFound
		}
		return db.AttributionReceipt{}, err
	}
	var receipt db.AttributionReceipt
	err := json.Unmarshal([]byte(raw), &receipt)
	return receipt, err
}

// RecordRootAttribution signs and retains a credential-bound creation receipt and its entity references.
func (d *Store) RecordRootAttribution(ctx context.Context, enrollmentID int64, event db.RemoteEvent, signer db.RootAttributionSigner) (db.AttributionReceipt, error) {
	var receipt db.AttributionReceipt
	err := d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		var err error
		receipt, err = d.recordRootAttributionTx(ctx, tx, enrollmentID, event, signer)
		return err
	})
	return receipt, err
}

func (d *Store) recordRootAttributionTx(ctx context.Context, tx *sql.Tx, enrollmentID int64, event db.RemoteEvent, signer db.RootAttributionSigner) (db.AttributionReceipt, error) {
	if !db.ProjectAttributionVisible(ctx, event.ProjectUID) {
		return db.AttributionReceipt{}, db.ErrNotFound
	}
	if _, _, err := db.ValidateRemoteEventContentHash(event); err != nil {
		return db.AttributionReceipt{}, err
	}
	var projectID int64
	var role string
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT p.id,b.role,b.enabled FROM projects p JOIN federation_bindings b ON b.project_id=p.id WHERE p.uid=? AND p.deleted_at IS NULL`, event.ProjectUID).Scan(&projectID, &role, &enabled); err != nil {
		return db.AttributionReceipt{}, err
	}
	if role != string(db.FederationRoleHub) || !enabled {
		return db.AttributionReceipt{}, errors.New("only an enabled root hub can issue attribution")
	}
	grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE id=?`, enrollmentID))
	if err != nil {
		return db.AttributionReceipt{}, err
	}
	if grant.ProjectID == nil || *grant.ProjectID != projectID || !strings.Contains(","+grant.Capabilities+",", ",push,") {
		return db.AttributionReceipt{}, db.ErrNotFound
	}
	if err := d.FederationEnrollmentTransactionFence(grant, projectID, "push")(ctx, tx); err != nil {
		return db.AttributionReceipt{}, err
	}
	if err := d.ProjectAccessTransactionFence(grant.Actor, []string{event.ProjectUID})(ctx, tx); err != nil {
		return db.AttributionReceipt{}, err
	}
	actor, ingress := grant.Actor, grant.SpokeInstanceUID

	return issueRootReceiptTx(ctx, tx, projectID, event, signer, actor, ingress, true)
}

func issueRootReceiptTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent, signer db.RootAttributionSigner, actor, ingress string, notifyUI bool) (db.AttributionReceipt, error) {
	pin, err := rootPinTx(ctx, tx, event.ProjectUID, "")
	if err != nil {
		return db.AttributionReceipt{}, err
	}
	if pin.AuthorityUID != signer.AuthorityUID {
		return db.AttributionReceipt{}, errors.New("signer does not match root authority")
	}
	var storedHash string
	if err := tx.QueryRowContext(ctx, `SELECT content_hash FROM events WHERE project_id=? AND uid=?`, projectID, event.EventUID).Scan(&storedHash); err != nil {
		return db.AttributionReceipt{}, err
	}
	if storedHash != event.ContentHash {
		return db.AttributionReceipt{}, db.ErrRemoteEventHashMismatch
	}
	previous, err := readReceipt(tx.QueryRowContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=? AND event_uid=?`, event.ProjectUID, event.EventUID))
	if err == nil {
		if previous.ContentHash != event.ContentHash {
			return db.AttributionReceipt{}, db.ErrRemoteEventHashMismatch
		}
		return previous, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return db.AttributionReceipt{}, err
	}
	var epoch, sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(reset_epoch),1) FROM federation_event_provenance WHERE project_uid=?`, event.ProjectUID).Scan(&epoch); err != nil {
		return db.AttributionReceipt{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM federation_event_provenance WHERE project_uid=? AND reset_epoch=?`, event.ProjectUID, epoch).Scan(&sequence); err != nil {
		return db.AttributionReceipt{}, err
	}
	handle, refs, err := db.EventCreationProvenance(event)
	if err != nil {
		return db.AttributionReceipt{}, err
	}
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: event.ProjectUID, EventUID: event.EventUID, ContentHash: event.ContentHash, AuthorityUID: pin.AuthorityUID, AccountableActor: actor, SourceActor: event.Actor, Teammate: handle, IngressInstanceUID: ingress, AcceptedAt: time.Now().UTC(), ResetEpoch: epoch, Sequence: sequence, KeyID: pin.KeyID}, signer.PrivateKey)
	if err != nil {
		return db.AttributionReceipt{}, err
	}
	if err := db.VerifyRootReceipt(pin, receipt); err != nil {
		return db.AttributionReceipt{}, err
	}
	if err := persistReceiptTx(ctx, tx, receipt, refs, notifyUI); err != nil {
		return db.AttributionReceipt{}, err
	}
	return receipt, nil
}

func persistReceiptTx(ctx context.Context, tx db.Transaction, receipt db.AttributionReceipt, refs []db.EntityProvenance, notifyUI bool) error {
	previous, err := readReceipt(tx.QueryRowContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=? AND event_uid=?`, receipt.ProjectUID, receipt.EventUID))
	if err == nil {
		oldJSON, _ := json.Marshal(previous)
		newJSON, _ := json.Marshal(receipt)
		if string(oldJSON) != string(newJSON) {
			return errors.New("conflicting immutable root receipt")
		}
	} else {
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES(?,?,?,?,?,?,?)`, receipt.ProjectUID, receipt.EventUID, receipt.ContentHash, receipt.ResetEpoch, receipt.Sequence, receipt.KeyID, string(raw)); err != nil {
			return err
		}
	}
	rawForRelay, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	var projectID int64
	var instanceUID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=?`, receipt.ProjectUID).Scan(&projectID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&instanceUID); err != nil {
		return err
	}
	if err := queueRelayBodyTx(ctx, tx, projectID, receipt.ProjectUID, db.RelayStreamReceipt, receipt.EventUID, receipt.ContentHash, rawForRelay, instanceUID); err != nil {
		return err
	}

	changed := false
	for _, ref := range refs {
		result, err := tx.ExecContext(ctx, `INSERT INTO federation_entity_provenance(project_uid,kind,entity_uid,event_uid) VALUES(?,?,?,?) ON CONFLICT(project_uid,kind,entity_uid) DO NOTHING`, ref.ProjectUID, ref.Kind, ref.EntityUID, ref.EventUID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = changed || count > 0
	}
	if notifyUI && changed {
		return reserveAttributionUIResetTx(ctx, tx, receipt.ProjectUID)
	}
	return nil
}

// ApplyUpstreamAttribution verifies and retains receipts received from the pinned upstream.
func (d *Store) ApplyUpstreamAttribution(ctx context.Context, pin db.RootKeyPin, receipt db.AttributionReceipt) error {
	if !db.ProjectAttributionVisible(ctx, receipt.ProjectUID) {
		return db.ErrNotFound
	}
	return d.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		actual, err := rootPinTx(ctx, tx, receipt.ProjectUID, receipt.KeyID)
		if err != nil {
			return err
		}
		if pin.AuthorityUID != actual.AuthorityUID || pin.KeyID != actual.KeyID || pin.ProjectUID != actual.ProjectUID {
			return errors.New("upstream pin does not match enrolled root")
		}
		if err := db.VerifyRootReceipt(actual, receipt); err != nil {
			return err
		}
		var projectID int64
		var role, actor, hubProjectUID string
		var enabled bool
		if err := tx.QueryRowContext(ctx, `SELECT p.id,b.role,b.enabled,b.bound_actor,b.hub_project_uid FROM projects p JOIN federation_bindings b ON b.project_id=p.id WHERE p.uid=? AND p.deleted_at IS NULL`, receipt.ProjectUID).Scan(&projectID, &role, &enabled, &actor, &hubProjectUID); err != nil {
			return err
		}
		if role != string(db.FederationRoleSpoke) || !enabled || hubProjectUID != receipt.ProjectUID {
			return errors.New("root receipts require an active pinned upstream binding")
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, projectID))
		if err != nil {
			return err
		}
		if binding.RelayConfig != nil {
			grant, err := d.upstreamRelayGrantTx(ctx, tx, binding.RelayConfig.BindingUID)
			if err != nil {
				return err
			}
			actor = grant.Actor
		}
		if err := d.ProjectAccessTransactionFence(actor, []string{receipt.ProjectUID})(ctx, tx); err != nil {
			return err
		}
		// A receipt can precede its source event. Keep it durably and attach creation
		// when that immutable source arrives; reset manifests carry explicit refs.
		event, err := attributionSourceTx(ctx, tx, projectID, receipt)
		var refs []db.EntityProvenance
		if err == nil {
			if event.ContentHash != receipt.ContentHash || event.Actor != receipt.SourceActor {
				return db.ErrRemoteEventHashMismatch
			}
			handle, creation, e := db.EventCreationProvenance(event)
			if e != nil {
				return e
			}
			if handle != receipt.Teammate {
				return errors.New("receipt teammate disagrees with source")
			}
			refs = creation
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return persistReceiptTx(ctx, tx, receipt, refs, true)
	})
}

// EntityAttribution reads an entity’s retained creation receipt.
func (d *Store) EntityAttribution(ctx context.Context, projectUID, kind, entityUID string) (db.AttributionReceipt, error) {
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return db.AttributionReceipt{}, db.ErrNotFound
	}
	return readReceipt(d.QueryRowContext(ctx, `SELECT p.receipt FROM federation_entity_provenance e JOIN federation_event_provenance p ON p.project_uid=e.project_uid AND p.event_uid=e.event_uid WHERE e.project_uid=? AND e.kind=? AND e.entity_uid=?`, projectUID, kind, entityUID))
}

// AttributionReceiptsAfter reads the next bounded prefix of retained attribution receipts.
func (d *Store) AttributionReceiptsAfter(ctx context.Context, projectUID string, epoch, after int64, limit int) ([]db.AttributionReceipt, error) {
	result := make([]db.AttributionReceipt, 0)
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return result, nil
	}
	if epoch <= 0 || after < 0 || limit <= 0 || limit > 1000 {
		return nil, errors.New("invalid receipt cursor or limit")
	}
	rows, err := d.QueryContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=? AND reset_epoch=? AND sequence>? ORDER BY sequence LIMIT ?`, projectUID, epoch, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		receipt, err := readReceipt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, receipt)
	}
	return result, rows.Err()
}

// ExportAttribution exports public pins, receipts and creation references within the selected scope.
func (d *Store) ExportAttribution(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.ImportRecord, error] {
	return func(yield func(db.ImportRecord, error) bool) {
		for _, kind := range []string{"federation_root_key", "federation_event_provenance", "federation_entity_provenance"} {
			var query string
			switch kind {
			case "federation_root_key":
				query = `SELECT k.project_uid,k.authority_uid,k.key_id,k.public_key,k.active FROM federation_root_keys k JOIN projects p ON p.uid=k.project_uid`
			case "federation_event_provenance":
				query = `SELECT k.receipt FROM federation_event_provenance k JOIN projects p ON p.uid=k.project_uid`
			default:
				query = `SELECT k.project_uid,k.kind,k.entity_uid,k.event_uid FROM federation_entity_provenance k JOIN projects p ON p.uid=k.project_uid`
			}
			args := []any{}
			where := []string{}
			if kind == "federation_entity_provenance" {
				deleted := ""
				if !filter.IncludeDeleted {
					deleted = " AND i.deleted_at IS NULL"
				}
				where = append(where, "((k.kind='issue' AND EXISTS(SELECT 1 FROM issues i WHERE i.uid=k.entity_uid AND i.project_id=p.id"+deleted+")) OR (k.kind='comment' AND EXISTS(SELECT 1 FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=k.entity_uid AND i.project_id=p.id"+deleted+")))")
			}
			if filter.ProjectID != nil {
				where = append(where, `p.id=?`)
				args = append(args, *filter.ProjectID)
			}
			if !filter.IncludeDeleted {
				if kind == "federation_root_key" {
					where = append(where, `(p.deleted_at IS NULL OR EXISTS(SELECT 1 FROM federation_bindings b WHERE b.project_id=p.id))`)
				} else {
					where = append(where, `p.deleted_at IS NULL`)
				}
			}
			allowed, restricted := db.AuthorizedProjects(ctx)
			if restricted {
				if len(allowed) == 0 {
					return
				}
				places := make([]string, len(allowed))
				for i, value := range allowed {
					places[i] = "?"
					args = append(args, value)
				}
				where = append(where, "p.uid IN ("+strings.Join(places, ",")+")")
			}
			if len(where) > 0 {
				query += " WHERE " + strings.Join(where, " AND ")
			}
			switch kind {
			case "federation_root_key":
				query += " ORDER BY k.project_uid,k.key_id"
			case "federation_event_provenance":
				query += " ORDER BY k.project_uid,k.reset_epoch,k.sequence"
			default:
				query += " ORDER BY k.project_uid,k.kind,k.entity_uid"
			}
			rows, err := d.readQ.QueryContext(ctx, query, args...)
			if err != nil {
				yield(nil, err)
				return
			}
			keep := true
			for rows.Next() {
				var record db.ImportRecord
				switch kind {
				case "federation_root_key":
					pin, e := scanRootPin(rows)
					if e != nil {
						yield(nil, e)
						_ = rows.Close()
						return
					}
					record = &pin
				case "federation_event_provenance":
					receipt, e := readReceipt(rows)
					if e != nil {
						yield(nil, e)
						_ = rows.Close()
						return
					}
					record = &receipt
				default:
					var ref db.EntityProvenance
					if e := rows.Scan(&ref.ProjectUID, &ref.Kind, &ref.EntityUID, &ref.EventUID); e != nil {
						yield(nil, e)
						_ = rows.Close()
						return
					}
					record = &ref
				}
				if !yield(record, nil) {
					keep = false
					break
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				yield(nil, fmt.Errorf("export root provenance: %w", err))
				return
			}
			if !keep {
				return
			}
		}
	}
}

var _ db.AttributionStorage = (*Store)(nil)

// Native root writes carry trusted request authority. Replica/relay writes stay
// pending for their enrolled root; no descendant may issue root proof locally.
func (d *Store) recordNativeRootAttributionTx(ctx context.Context, tx *sql.Tx, event db.Event) error {
	signer, actor, ok := db.RootAttributionFromContext(ctx)
	if !ok || event.OriginInstanceUID != d.instanceUID {
		return nil
	}
	var role string
	var enabled bool
	err := tx.QueryRowContext(ctx, `SELECT role,enabled FROM federation_bindings WHERE project_id=?`, event.ProjectID).Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if role != string(db.FederationRoleHub) || !enabled {
		return nil
	}
	pin, err := rootPinTx(ctx, tx, event.ProjectUID, "")
	if err == nil && pin.AuthorityUID != d.instanceUID {
		return nil
	}
	if signer.AuthorityUID != d.instanceUID {
		return errors.New("native signing authority does not match local root")
	}
	if errors.Is(err, db.ErrNotFound) {
		if len(signer.PrivateKey) != ed25519.PrivateKeySize {
			return errors.New("invalid native root signing key")
		}
		public := signer.PrivateKey.Public().(ed25519.PublicKey)
		pin = db.RootKeyPin{ProjectUID: event.ProjectUID, AuthorityUID: signer.AuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
		if err = db.ValidateRootKeyPin(pin); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES(?,?,?, ?,1)`, pin.ProjectUID, pin.AuthorityUID, pin.KeyID, base64.StdEncoding.EncodeToString(pin.PublicKey))
	}
	if err != nil {
		return err
	}
	if db.RootAttributionOwner(ctx) {
		actor = event.Actor // Existing actor convention on an admitted owner boundary.
	} else if err := d.ProjectAccessTransactionFence(actor, []string{event.ProjectUID})(ctx, tx); err != nil {
		return err
	}
	_, err = issueRootReceiptTx(ctx, tx, event.ProjectID, db.RemoteEventFromStored(event), signer, actor, d.instanceUID, false)
	return err
}

// A receipt can be durable before its source stream catches up. Verify its
// immutable commitment before committing the source, then attach creation in
// that same transaction. Normal receipt delivery handles the opposite order.
func (d *Store) attachStoredRootReceiptTx(ctx context.Context, tx *sql.Tx, event db.Event) error {
	receipt, err := readReceipt(tx.QueryRowContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=? AND event_uid=?`, event.ProjectUID, event.UID))
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !db.ProjectAttributionVisible(ctx, event.ProjectUID) {
		return db.ErrNotFound
	}
	pin, err := rootPinTx(ctx, tx, event.ProjectUID, receipt.KeyID)
	if err != nil {
		return err
	}
	if err := db.VerifyRootReceipt(pin, receipt); err != nil {
		return err
	}
	if event.ContentHash != receipt.ContentHash || event.Actor != receipt.SourceActor {
		return db.ErrRemoteEventHashMismatch
	}
	var role, actor, upstreamProjectUID string
	var enabled bool
	err = tx.QueryRowContext(ctx, `SELECT role,bound_actor,enabled,hub_project_uid FROM federation_bindings WHERE project_id=?`, event.ProjectID).Scan(&role, &actor, &enabled, &upstreamProjectUID)
	if err != nil {
		return err
	}
	if role == string(db.FederationRoleSpoke) {
		if !enabled || upstreamProjectUID != event.ProjectUID {
			return db.ErrNotFound
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, event.ProjectID))
		if err != nil {
			return err
		}
		if binding.RelayConfig != nil {
			grant, err := d.upstreamRelayGrantTx(ctx, tx, binding.RelayConfig.BindingUID)
			if err != nil {
				return err
			}
			actor = grant.Actor
		}
		if err := d.ProjectAccessTransactionFence(actor, []string{event.ProjectUID})(ctx, tx); err != nil {
			return err
		}
	}
	handle, refs, err := db.EventCreationProvenance(db.RemoteEventFromStored(event))
	if err != nil {
		return err
	}
	if handle != receipt.Teammate {
		return errors.New("receipt teammate disagrees with arriving source")
	}
	return persistReceiptTx(ctx, tx, receipt, refs, false)
}
