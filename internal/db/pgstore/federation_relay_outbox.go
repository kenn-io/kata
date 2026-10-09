package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// PendingRelayDeliveries durably marks the selected prefix emitted before the
// caller can send it. Lost responses replay its retained bytes and identities.
func (d *Store) PendingRelayDeliveries(ctx context.Context, bindingUID, stream string, limit int) ([]db.RelayEnvelope, error) {
	if limit <= 0 || limit > 1024 || (stream != db.RelayStreamEvent && stream != db.RelayStreamReceipt && stream != db.RelayStreamArtifact) {
		return nil, fmt.Errorf("%w: invalid relay batch limit or stream", db.ErrFederationIngestValidation)
	}
	if stream == db.RelayStreamArtifact {
		limit = min(limit, 32)
	}
	var output []db.RelayEnvelope
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		output = []db.RelayEnvelope{}
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "pull")
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,envelope FROM federation_relay_outbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND acknowledged=0 ORDER BY id LIMIT $4`, bindingUID, stream, grant.RelayResetEpoch, limit)
		if err != nil {
			return err
		}
		var ids []int64
		total := 0
		manifestLimit := db.MaxRelayEnvelopeBytes
		if stream == db.RelayStreamArtifact {
			manifestLimit = 2 << 20
		}
		var declaredBytes int64
		for rows.Next() {
			var id int64
			var raw string
			if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			var e db.RelayEnvelope
			if err = json.Unmarshal([]byte(raw), &e); err != nil {
				break
			}
			if total+len(e.Body) > manifestLimit {
				break
			}
			if stream == db.RelayStreamArtifact {
				var manifest embedding.ArtifactManifest
				if json.Unmarshal(e.Body, &manifest, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifactManifest(manifest) != nil || !db.ValidRelayArtifactSourceUID(e.SourceUID, manifest.Digest) || manifest.Digest != e.SourceHash || manifest.ProjectUID != e.ProjectUID {
					err = db.ErrFederationIngestValidation
					break
				}
				if manifest.VectorByteSize > embedding.MaxArtifactBatchBytes-declaredBytes {
					break
				}
				declaredBytes += manifest.VectorByteSize
			}
			total += len(e.Body)
			output = append(output, e)
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE federation_relay_outbox SET emitted=1 WHERE id=$1`, id); err != nil {
				return err
			}
		}
		if len(ids) > 0 {
			var projectUID string
			if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, *grant.ProjectID).Scan(&projectUID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_cursors(project_uid,binding_uid,stream,reset_epoch,emitted_through) VALUES($1,$2,$3,$4,$5) ON CONFLICT(binding_uid,stream,reset_epoch) DO UPDATE SET emitted_through=GREATEST(federation_relay_cursors.emitted_through,excluded.emitted_through)`, projectUID, bindingUID, stream, grant.RelayResetEpoch, ids[len(ids)-1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}

// AckRelayDeliveries accepts only the exact digest of an emitted endpoint and
// refuses to skip any earlier un-emitted row. Sequence gaps from other streams
// are legal; arithmetic contiguity is never assumed.
func (d *Store) AckRelayDeliveries(ctx context.Context, bindingUID string, epoch int64, stream string, through int64, digest string) error {
	ctx = db.WithRelayRequestedEpoch(ctx, epoch)
	return d.relayTx(ctx, func(tx *sql.Tx) error {
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "pull")
		if err != nil {
			return err
		}
		if epoch != grant.RelayResetEpoch || through <= 0 {
			return fmt.Errorf("%w: invalid relay ack epoch or endpoint", db.ErrFederationIngestValidation)
		}
		var raw string
		var emitted int
		if err := tx.QueryRowContext(ctx, `SELECT envelope,emitted FROM federation_relay_outbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND id=$4`, bindingUID, stream, epoch, through).Scan(&raw, &emitted); err != nil {
			return err
		}
		var e db.RelayEnvelope
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return err
		}
		if emitted != 1 || e.Digest != digest {
			return fmt.Errorf("%w: relay ack does not identify an emitted envelope", db.ErrFederationIngestValidation)
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_relay_outbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND id<=$4 AND emitted=0`, bindingUID, stream, epoch, through).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("%w: relay ack skips an un-emitted delivery", db.ErrFederationIngestValidation)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_relay_outbox SET acknowledged=1 WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND id<=$4`, bindingUID, stream, epoch, through); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE federation_relay_cursors SET acknowledged_through=GREATEST(acknowledged_through,$1) WHERE binding_uid=$2 AND stream=$3 AND reset_epoch=$4`, through, bindingUID, stream, epoch)
		return err
	})
}

func (d *Store) relayGrantTx(ctx context.Context, tx *sql.Tx, bindingUID, capability string) (db.FederationEnrollment, error) {
	if err := lockProjectAccess(ctx, tx); err != nil {
		return db.FederationEnrollment{}, err
	}
	// Discovery before the binding lock is not admission; the fence rechecks the
	// exact grant under the binding-first lock order before any data is exposed.
	grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE relay_binding_uid=$1`, bindingUID))
	if errors.Is(err, db.ErrNotFound) {
		return d.upstreamRelayGrantTx(ctx, tx, bindingUID)
	}
	if err != nil {
		return grant, err
	}
	if grant.ProjectID == nil || grant.RelayProtocolVersion != db.RelayProtocolVersion {
		return grant, db.ErrNotFound
	}
	if err = d.FederationEnrollmentTransactionFence(grant, *grant.ProjectID, capability)(ctx, tx); err != nil {
		return db.FederationEnrollment{}, err
	}
	return d.activateRelayCheckpointTx(ctx, tx, grant)
}

// queueRelaySourceTx is called by the shared source-event insertion path, in
// the transaction which owns both the event and its projected mutation.
func (d *Store) queueRelaySourceTx(ctx context.Context, tx *sql.Tx, event db.Event) error {
	// Local catalog, transport and claim audits are not shared content.
	// Unknown data events still retain delivery intent for quarantine handling.
	if db.RelayEventIsLocalAudit(event.Type) {
		return nil
	}
	if err := d.retainPendingCreationTx(ctx, tx, event); err != nil {
		return err
	}
	body, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(event))
	if err != nil {
		return err
	}
	if err := queueRelayBodyTx(ctx, tx, event.ProjectID, event.ProjectUID, db.RelayStreamEvent, event.UID, event.ContentHash, body, d.instanceUID); err != nil {
		return err
	}
	if event.Type == "issue.restored" && event.IssueUID != nil {
		return d.queueRestoredArtifactsTx(ctx, tx, event)
	}
	return nil
}

// queueRelayBodyTx retains immutable bodies even after acknowledgment, so event
// compaction and lost replies cannot change retry identity or creation history.
func queueRelayBodyTx(ctx context.Context, tx db.Transaction, projectID int64, projectUID, stream, sourceUID, sourceHash string, body []byte, instanceUID string, selected ...db.FederationEnrollment) error {
	if db.RelayStateCapture(ctx) {
		return nil
	}
	if stream == db.RelayStreamArtifact {
		if forwarded := db.RelayArtifactSourceUID(ctx); forwarded != "" {
			sourceUID = forwarded
		} else if sourceUID == sourceHash {
			var manifest embedding.ArtifactManifest
			if err := json.Unmarshal(body, &manifest); err != nil {
				return err
			}
			var restoredBy string
			err := tx.QueryRowContext(ctx, `SELECT uid FROM events WHERE project_id=$1 AND issue_uid=$2 AND type='issue.restored' ORDER BY id DESC LIMIT 1`, projectID, manifest.IssueUID).Scan(&restoredBy)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				sourceUID = sourceHash + ":" + restoredBy
			}
		}
	}
	grants := selected
	if len(selected) == 0 {
		queryer, ok := tx.(interface {
			QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		})
		if !ok {
			return errors.New("relay transaction cannot list enrollments")
		}
		rows, err := queryer.QueryContext(ctx, federationEnrollmentSelect+` WHERE project_id=$1 AND relay_protocol_version=1 AND revoked_at IS NULL ORDER BY id`, projectID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var grant db.FederationEnrollment
			grant, err = scanFederationEnrollment(rows)
			if err != nil {
				break
			}
			grants = append(grants, grant)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, projectID))
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
		// Retain local write intent while transport is paused or revoked.
		// Emission and acceptance still require the current upstream grant.
		if err == nil && binding.Role == db.FederationRoleSpoke && binding.RelayConfig != nil {
			c := binding.RelayConfig
			if err := c.Validate(instanceUID); err != nil {
				return err
			}
			grants = append(grants, db.FederationEnrollment{ProjectID: &projectID, RelayBindingUID: c.BindingUID, RelayProtocolVersion: c.ProtocolVersion, RelayResetEpoch: c.ResetEpoch, SpokeInstanceUID: c.UpstreamInstanceUID})
		}
	}
	if len(grants) == 0 {
		return nil
	}
	pin, err := rootPinTx(ctx, tx, projectUID, "")
	if err != nil {
		return err
	}
	path := db.RelayForwardPath(ctx)
	if len(path) == 0 {
		path = []string{instanceUID}
		if stream == db.RelayStreamEvent {
			source, err := db.DecodeRelaySourceEvent(body)
			if err != nil {
				return err
			}
			if source.OriginInstanceUID != instanceUID {
				path = []string{source.OriginInstanceUID, instanceUID}
			}
		}
	}
	for _, grant := range grants {
		if slices.Contains(path, grant.SpokeInstanceUID) {
			continue
		}
		if stream == db.RelayStreamEvent {
			source, err := db.DecodeRelaySourceEvent(body)
			if err != nil {
				return err
			}
			if source.OriginInstanceUID == grant.SpokeInstanceUID {
				continue
			}
		}
		var id int64
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_outbox(project_uid,binding_uid,stream,reset_epoch,source_uid,source_hash,envelope) VALUES($1,$2,$3,$4,$5,$6,'') ON CONFLICT(binding_uid,stream,reset_epoch,source_uid) DO NOTHING`, projectUID, grant.RelayBindingUID, stream, grant.RelayResetEpoch, sourceUID, sourceHash); err != nil {
			return err
		}
		var retained string
		if err := tx.QueryRowContext(ctx, `SELECT id,envelope FROM federation_relay_outbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND source_uid=$4`, grant.RelayBindingUID, stream, grant.RelayResetEpoch, sourceUID).Scan(&id, &retained); err != nil {
			return err
		}
		if retained != "" {
			var old db.RelayEnvelope
			if err := json.Unmarshal([]byte(retained), &old); err != nil {
				return err
			}
			if old.SourceHash != sourceHash {
				return fmt.Errorf("conflicting relay source %s", sourceUID)
			}
			continue
		}
		authority := db.RelayHopAuthority{BindingUID: grant.RelayBindingUID, ProjectUID: projectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: instanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: grant.RelayResetEpoch}
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: authority.BindingUID, ProjectUID: authority.ProjectUID, AuthorityUID: authority.AuthorityUID, SenderInstanceUID: authority.SenderInstanceUID, ReceiverInstanceUID: authority.ReceiverInstanceUID, Epoch: authority.Epoch, Sequence: id, Stream: stream, Path: path, SourceUID: sourceUID, SourceHash: sourceHash, Body: body})
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_relay_outbox SET envelope=$1 WHERE id=$2`, string(encoded), id); err != nil {
			return err
		}
	}
	return nil
}
