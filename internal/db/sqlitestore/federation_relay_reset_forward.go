package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// forwardRelayResetTx reuses a durably installed root checkpoint. Only the
// immediate-hop translation changes, under the same live enrollment fence.
func (d *Store) forwardRelayResetTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, pin db.RootKeyPin) (db.RelayResetCheckpoint, error) {
	binding, _, err := d.relayServingBindingTx(ctx, tx, *grant.ProjectID, db.RelayProtocolVersion)
	if err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	if binding.RelayConfig == nil {
		return db.RelayResetCheckpoint{}, db.ErrNotFound
	}
	upstream, err := d.upstreamRelayGrantTx(ctx, tx, binding.RelayConfig.BindingUID)
	if err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, db.RelayResetMetadataPrefix+pin.ProjectUID).Scan(&raw); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	var checkpoint db.RelayResetCheckpoint
	if err := json.Unmarshal([]byte(raw), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	historical, err := rootPinTx(ctx, tx, pin.ProjectUID, checkpoint.Manifest.KeyID)
	if err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	if historical.AuthorityUID != pin.AuthorityUID {
		return db.RelayResetCheckpoint{}, errors.New("forwarded reset changes root authority")
	}
	if err := db.VerifyRootResetManifest(historical, checkpoint.Manifest, checkpoint.Snapshot); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	authority := db.RelayHopAuthority{BindingUID: binding.RelayConfig.BindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: upstream.SpokeInstanceUID, ReceiverInstanceUID: d.instanceUID, Epoch: upstream.RelayResetEpoch}
	if err := db.ValidateRelayResetTranslation(authority, checkpoint.Manifest, checkpoint.Translation); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	if _, _, err := db.DecodeRootResetPayload(historical, checkpoint.Snapshot); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	upstreamBaselines := checkpoint.Translation.HopBaselines
	newEpoch := grant.RelayResetEpoch + 1
	checkpoint.Translation = db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: grant.RelayBindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: d.instanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: newEpoch}, SnapshotUID: checkpoint.Manifest.SnapshotUID, SnapshotDigest: checkpoint.Manifest.SnapshotDigest}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, db.RelayResetMetadataPrefix+pin.ProjectUID+"."+grant.RelayBindingUID, string(encoded)); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	grant.RelayResetEpoch = newEpoch
	if err := d.seedRelayArtifactManifestsTx(ctx, tx, grant, pin.ProjectUID); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	if err := d.seedRelayPostCheckpointDeliveriesTx(ctx, tx, grant, binding.RelayConfig.BindingUID, upstream, pin, upstreamBaselines); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	if _, err := d.relayGrantTx(ctx, tx, grant.RelayBindingUID, "pull"); err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	return checkpoint, nil
}

// seedRelayPostCheckpointDeliveriesTx carries accepted upstream event and
// receipt envelopes beyond the installed checkpoint into this child's epoch.
// They remain ordinary post-snapshot deliveries and keep their original source
// bytes while receiving the child's immediate-hop identity.
func (d *Store) seedRelayPostCheckpointDeliveriesTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, upstreamBindingUID string, upstream db.FederationEnrollment, pin db.RootKeyPin, baselines db.RelayStreamCursors) error {
	if grant.ProjectID == nil {
		return db.ErrNotFound
	}
	authority := db.RelayHopAuthority{BindingUID: upstreamBindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: upstream.SpokeInstanceUID, ReceiverInstanceUID: d.instanceUID, Epoch: upstream.RelayResetEpoch}
	streams := []struct {
		name     string
		baseline int64
	}{{db.RelayStreamEvent, baselines.Events}, {db.RelayStreamReceipt, baselines.Receipts}}
	for _, stream := range streams {
		after := stream.baseline
		for {
			rows, err := tx.QueryContext(ctx, `SELECT sequence,source_uid,source_hash,envelope FROM federation_relay_inbox WHERE project_uid=? AND binding_uid=? AND stream=? AND reset_epoch=? AND sequence>? AND accepted=1 ORDER BY sequence LIMIT 100`, pin.ProjectUID, upstreamBindingUID, stream.name, upstream.RelayResetEpoch, after)
			if err != nil {
				return err
			}
			type retainedEnvelope struct {
				sequence      int64
				sourceUID     string
				sourceHash    string
				envelopeBytes string
			}
			var page []retainedEnvelope
			for rows.Next() {
				var item retainedEnvelope
				if err = rows.Scan(&item.sequence, &item.sourceUID, &item.sourceHash, &item.envelopeBytes); err != nil {
					break
				}
				page = append(page, item)
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			for _, item := range page {
				var envelope db.RelayEnvelope
				if err := json.Unmarshal([]byte(item.envelopeBytes), &envelope, json.RejectUnknownMembers(true)); err != nil {
					return err
				}
				if envelope.Sequence != item.sequence || envelope.Stream != stream.name || envelope.SourceUID != item.sourceUID || envelope.SourceHash != item.sourceHash || db.ValidateRelayEnvelope(authority, envelope) != nil {
					return fmt.Errorf("%w: retained upstream relay envelope is inconsistent", db.ErrFederationIngestValidation)
				}
				if stream.name == db.RelayStreamEvent {
					source, err := db.DecodeRelaySourceEvent(envelope.Body)
					if err != nil || source.ProjectUID != pin.ProjectUID || source.EventUID != item.sourceUID || source.ContentHash != item.sourceHash {
						return fmt.Errorf("%w: retained upstream relay source is inconsistent", db.ErrFederationIngestValidation)
					}
				}
				path := append(append([]string(nil), envelope.Path...), d.instanceUID)
				offerCtx := db.WithRelayForwardPath(ctx, path)
				if err := queueRelayBodyTx(offerCtx, tx, *grant.ProjectID, pin.ProjectUID, stream.name, item.sourceUID, item.sourceHash, envelope.Body, d.instanceUID, grant); err != nil {
					return err
				}
				after = item.sequence
			}
		}
	}
	// The upstream inbox contains root-origin deliveries, but local and other
	// downstream-origin events are retained in the outgoing upstream outbox.
	// Those envelopes already carry this hub in their relay path, so preserve
	// their path and body exactly when seeding a newly enrolled child.
	outboundAuthority := db.RelayHopAuthority{BindingUID: upstreamBindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: d.instanceUID, ReceiverInstanceUID: upstream.SpokeInstanceUID, Epoch: upstream.RelayResetEpoch}
	var after int64
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id,source_uid,source_hash,envelope FROM federation_relay_outbox WHERE project_uid=? AND binding_uid=? AND stream=? AND reset_epoch=? AND id>? ORDER BY id LIMIT 100`, pin.ProjectUID, upstreamBindingUID, db.RelayStreamEvent, upstream.RelayResetEpoch, after)
		if err != nil {
			return err
		}
		type retainedEvent struct {
			sequence      int64
			sourceUID     string
			sourceHash    string
			envelopeBytes string
		}
		var page []retainedEvent
		for rows.Next() {
			var item retainedEvent
			if err = rows.Scan(&item.sequence, &item.sourceUID, &item.sourceHash, &item.envelopeBytes); err != nil {
				break
			}
			page = append(page, item)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			var envelope db.RelayEnvelope
			if err := json.Unmarshal([]byte(item.envelopeBytes), &envelope, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			if envelope.Sequence != item.sequence || envelope.Stream != db.RelayStreamEvent || envelope.SourceUID != item.sourceUID || envelope.SourceHash != item.sourceHash || db.ValidateRelayEnvelope(outboundAuthority, envelope) != nil {
				return fmt.Errorf("%w: retained outgoing relay envelope is inconsistent", db.ErrFederationIngestValidation)
			}
			source, err := db.DecodeRelaySourceEvent(envelope.Body)
			if err != nil || source.ProjectUID != pin.ProjectUID || source.EventUID != item.sourceUID || source.ContentHash != item.sourceHash || source.OriginInstanceUID != envelope.Path[0] {
				return fmt.Errorf("%w: retained outgoing relay source is inconsistent", db.ErrFederationIngestValidation)
			}
			offerCtx := db.WithRelayForwardPath(ctx, envelope.Path)
			if err := queueRelayBodyTx(offerCtx, tx, *grant.ProjectID, pin.ProjectUID, db.RelayStreamEvent, item.sourceUID, item.sourceHash, envelope.Body, d.instanceUID, grant); err != nil {
				return err
			}
			after = item.sequence
		}
	}
	return nil
}
