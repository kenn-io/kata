package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"

	"go.kenn.io/kata/internal/db"
)

// Preparing a checkpoint does not retire the peer's old namespace. The first
// authenticated new-epoch operation activates it; late old-epoch intent can
// still drain, preserving its emitted envelopes and source identity.
func (d *Store) activateRelayCheckpointTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment) (db.FederationEnrollment, error) {
	requested := db.RelayRequestedEpoch(ctx)
	if requested == 0 || requested == grant.RelayResetEpoch {
		return grant, nil
	}
	if requested != grant.RelayResetEpoch+1 {
		return grant, db.ErrFederationIngestValidation
	}
	var projectUID, raw string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, *grant.ProjectID).Scan(&projectUID); err != nil {
		return grant, err
	}
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return grant, db.ErrNotFound
	}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, db.RelayResetMetadataPrefix+projectUID+"."+grant.RelayBindingUID).Scan(&raw); err != nil {
		return grant, db.ErrFederationIngestValidation
	}
	var checkpoint db.RelayResetCheckpoint
	if err := json.Unmarshal([]byte(raw), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
		return grant, err
	}
	pin, err := rootPinTx(ctx, tx, projectUID, checkpoint.Manifest.KeyID)
	if err != nil {
		return grant, err
	}
	authority := db.RelayHopAuthority{BindingUID: grant.RelayBindingUID, ProjectUID: projectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: d.instanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: requested}
	if err := db.ValidateRelayResetTranslation(authority, checkpoint.Manifest, checkpoint.Translation); err != nil {
		return grant, err
	}
	if err := db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot); err != nil {
		return grant, err
	}
	// Writes committed after preparation remain owed. Reoffer them in the new
	// namespace; leave old emitted records untouched rather than fabricating ACKs.
	rows, err := tx.QueryContext(ctx, `SELECT envelope FROM federation_relay_outbox WHERE binding_uid=$1 AND reset_epoch=$2 AND acknowledged=0 ORDER BY id`, grant.RelayBindingUID, grant.RelayResetEpoch)
	if err != nil {
		return grant, err
	}
	var retained []db.RelayEnvelope
	for rows.Next() {
		var bytes string
		if err = rows.Scan(&bytes); err != nil {
			break
		}
		var envelope db.RelayEnvelope
		if err = json.Unmarshal([]byte(bytes), &envelope, json.RejectUnknownMembers(true)); err != nil {
			break
		}
		retained = append(retained, envelope)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return grant, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE federation_enrollments SET relay_reset_epoch=$1 WHERE id=$2`, requested, grant.ID); err != nil {
		return grant, err
	}
	grant.RelayResetEpoch = requested
	for _, envelope := range retained {
		if err := queueRelayBodyTx(db.WithRelayArtifactSourceUID(db.WithRelayForwardPath(ctx, envelope.Path), envelope.SourceUID), tx, *grant.ProjectID, projectUID, envelope.Stream, envelope.SourceUID, envelope.SourceHash, envelope.Body, d.instanceUID, grant); err != nil {
			return grant, err
		}
	}
	return grant, nil
}
