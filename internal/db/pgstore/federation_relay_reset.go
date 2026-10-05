package pgstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// InstallRelayReset verifies the pinned root and current upstream authority
// inside the same transaction that replaces the projection and stream baselines.
// Outstanding deliveries and descendants block replacement before any mutation.
func (d *Store) InstallRelayReset(ctx context.Context, bindingUID string, manifest db.RootResetManifest, snapshot db.RootResetSnapshot, translation db.RelayResetTranslation) error {
	total := len(snapshot.Events) + len(snapshot.Entities) + len(snapshot.Provenance) + len(snapshot.Artifacts)
	if total > db.MaxRelayEnvelopeBytes {
		return fmt.Errorf("%w: reset exceeds byte limit", db.ErrFederationIngestValidation)
	}
	return d.relayTx(ctx, func(tx *sql.Tx) error {
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "push")
		if err != nil {
			return err
		}
		if grant.ID != 0 {
			return errors.New("reset installation requires the configured upstream hop")
		}
		projectID := *grant.ProjectID
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID); err != nil {
			return err
		}
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			return db.ErrNotFound
		}
		pin, err := rootPinTx(ctx, tx, projectUID, "")
		if err != nil {
			return err
		}
		historical, err := rootPinTx(ctx, tx, projectUID, manifest.KeyID)
		if err != nil {
			return err
		}
		if historical.AuthorityUID != pin.AuthorityUID {
			return errors.New("reset changes pinned root authority")
		}
		if err := db.VerifyRootResetManifest(historical, manifest, snapshot); err != nil {
			return err
		}
		authority := db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: projectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: grant.SpokeInstanceUID, ReceiverInstanceUID: d.instanceUID, Epoch: translation.Authority.Epoch}
		if err := db.ValidateRelayResetTranslation(authority, manifest, translation); err != nil {
			return err
		}
		events, provenance, err := db.DecodeRootResetPayload(historical, snapshot)
		if err != nil {
			return err
		}
		events, err = d.retainPostCheckpointRelayEventsTx(ctx, tx, projectID, projectUID, historical, events, &provenance)
		if err != nil {
			return err
		}
		checkpoint := db.RelayResetCheckpoint{Manifest: manifest, Snapshot: snapshot, Translation: translation}
		raw, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		var previous string
		err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, db.RelayResetMetadataPrefix+projectUID).Scan(&previous)
		if err == nil {
			if previous == string(raw) && translation.Authority.Epoch == grant.RelayResetEpoch {
				return nil
			}
			var old db.RelayResetCheckpoint
			if err := json.Unmarshal([]byte(previous), &old, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			oldManifest, err := json.Marshal(old.Manifest)
			if err != nil {
				return err
			}
			newManifest, err := json.Marshal(manifest)
			if err != nil {
				return err
			}
			if manifest.ResetEpoch < old.Manifest.ResetEpoch || (manifest.ResetEpoch == old.Manifest.ResetEpoch && string(oldManifest) != string(newManifest)) {
				return errors.New("stale or conflicting root reset checkpoint")
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if translation.Authority.Epoch <= grant.RelayResetEpoch {
			return errors.New("stale immediate-hop reset epoch")
		}
		if err := d.validateRelayLifecycleTx(ctx, tx, projectID); err != nil {
			return err
		}
		var blocked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox o WHERE o.project_uid=$1 AND o.acknowledged=0 AND (o.binding_uid=$2 OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.relay_binding_uid=o.binding_uid AND e.relay_reset_epoch=o.reset_epoch AND e.revoked_at IS NULL)))`, projectUID, bindingUID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return db.ErrFederationResetBlockedByPendingPush
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_quarantine WHERE project_id=$1 AND skipped_at IS NULL)`, projectID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return db.ErrFederationResetBlockedByQuarantine
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_enrollments WHERE project_id=$1 AND revoked_at IS NULL AND relay_protocol_version=1)`, projectID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return errors.New("relay reset blocked by active downstream enrollments")
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM external_root_bindings WHERE project_id=$1)`, projectID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return db.ErrFederationResetBlockedByExternalRoot
		}
		known := map[string]struct{}{}
		for _, event := range events {
			rememberIngestIssueUIDs(event, known)
		}
		for _, event := range events {
			if err := validateFederationProjectEvent(projectUID, event.OriginInstanceUID, event, known, true); err != nil {
				return err
			}
		}
		// The reset's imported events must not be echoed as fresh local intent.
		// They are inserted with original IDs/hashes, independently of hop sequencing.
		if err := lockEventSequenceTx(ctx, tx); err != nil {
			return err
		}
		if err := clearFederatedProjectTx(ctx, tx, projectID); err != nil {
			return err
		}
		for _, table := range []string{"federation_entity_provenance", "federation_event_provenance"} {
			// #nosec G202 -- Table names come from the fixed local list; the project UID is a bound value.
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE project_uid=$1`, projectUID); err != nil {
				return err
			}
		}
		for _, key := range provenance.Keys {
			var retained db.RootKeyPin
			retained, err = rootPinTx(ctx, tx, projectUID, key.KeyID)
			if err == nil {
				if retained.AuthorityUID != key.AuthorityUID || retained.KeyID != key.KeyID {
					return errors.New("reset historical key conflicts with retained root")
				}
				continue
			}
			if !errors.Is(err, db.ErrNotFound) {
				return err
			}
			if !key.Retired {
				return errors.New("reset cannot replace the active root")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES($1,$2,$3,$4,0)`, projectUID, key.AuthorityUID, key.KeyID, base64.StdEncoding.EncodeToString(key.PublicKey)); err != nil {
				return err
			}
		}
		for _, event := range events {
			if _, err := tx.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,related_issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, event.EventUID, event.OriginInstanceUID, projectID, event.ProjectName, event.IssueUID, event.RelatedIssueUID, event.Type, event.Actor, string(event.Payload), event.HLCPhysicalMS, event.HLCCounter, event.ContentHash, event.CreatedAt.UTC().Format(db.EventTimestampFormat)); err != nil {
				return err
			}
		}
		if err := d.materializeFederatedProjectTx(ctx, tx, projectID, true, nil); err != nil {
			return err
		}
		// Projection replacement cannot leave portable bytes reachable for issues
		// excluded by the authoritative snapshot. Artifacts have a project FK,
		// so the issue deletion alone does not remove them.
		if _, err := tx.ExecContext(ctx, `DELETE FROM federation_embedding_artifacts WHERE project_uid=$1 AND NOT EXISTS(SELECT 1 FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=federation_embedding_artifacts.project_uid AND i.uid=federation_embedding_artifacts.issue_uid)`, projectUID); err != nil {
			return err
		}
		var artifactManifests []embedding.ArtifactManifest
		if err := json.Unmarshal(snapshot.Artifacts, &artifactManifests, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		for _, offered := range artifactManifests {
			var retained, title, body string
			err := tx.QueryRowContext(ctx, `SELECT a.artifact,i.title,i.body FROM federation_embedding_artifacts a JOIN issues i ON i.uid=a.issue_uid JOIN projects p ON p.id=i.project_id AND p.uid=a.project_uid WHERE a.project_uid=$1 AND a.digest=$2 AND (a.staging_expires_at IS NULL OR a.staging_expires_at>$3)`, projectUID, offered.Digest, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&retained, &title, &body)
			if errors.Is(err, sql.ErrNoRows) {
				continue // Metadata alone cannot supply missing or expired bytes.
			}
			if err != nil {
				return err
			}
			var artifact embedding.EmbeddingArtifact
			input := embedding.EmbedText(title, body)
			if offered.InputHash != embedding.ArtifactInputHash(input) {
				input = "" // Valid stale bytes remain portable, never active-index evidence.
			}
			if json.Unmarshal([]byte(retained), &artifact, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifact(artifact, input, "") != nil || !reflect.DeepEqual(artifact.Manifest(), offered) {
				return db.ErrFederationIngestValidation
			}
			if _, err := tx.ExecContext(ctx, `UPDATE federation_embedding_artifacts SET staging_expires_at=NULL WHERE project_uid=$1 AND digest=$2`, projectUID, offered.Digest); err != nil {
				return err
			}
		}
		for _, receipt := range provenance.Receipts {
			bytes, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES($1,$2,$3,$4,$5,$6,$7)`, projectUID, receipt.EventUID, receipt.ContentHash, receipt.ResetEpoch, receipt.Sequence, receipt.KeyID, string(bytes)); err != nil {
				return err
			}
		}
		for _, ref := range provenance.Entities {
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_entity_provenance(project_uid,kind,entity_uid,event_uid) VALUES($1,$2,$3,$4)`, projectUID, ref.Kind, ref.EntityUID, ref.EventUID); err != nil {
				return err
			}
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, projectID))
		if err != nil {
			return err
		}
		config := *binding.RelayConfig
		config.ResetEpoch = translation.Authority.Epoch
		configBytes, err := json.Marshal(config)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET relay_config=$1 WHERE project_id=$2`, string(configBytes), projectID); err != nil {
			return err
		}
		for stream, baseline := range map[string]int64{db.RelayStreamEvent: translation.HopBaselines.Events, db.RelayStreamReceipt: translation.HopBaselines.Receipts, db.RelayStreamArtifact: translation.HopBaselines.Artifacts} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_cursors(project_uid,binding_uid,stream,reset_epoch,offered_through,accepted_through) VALUES($1,$2,$3,$4,$5,$6)`, projectUID, bindingUID, stream, config.ResetEpoch, baseline, baseline); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, db.RelayResetMetadataPrefix+projectUID, string(raw)); err != nil {
			return err
		}
		if err := reserveAttributionUIResetTx(ctx, tx, projectUID); err != nil {
			return err
		}
		// Time-dependent authority is rechecked at commit; binding/policy row fences
		// held by upstreamRelayGrantTx retain the account and membership decision.
		_, err = d.upstreamRelayGrantTx(ctx, tx, bindingUID)
		return err
	})
}
