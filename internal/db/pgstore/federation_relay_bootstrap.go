package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// seedRelayEnrollmentTx retains the existing portable history for exactly the
// newly authorized child. Small read batches bound memory, and the enrollment
// and complete outbox seed commit together. Compacted history uses signed reset.
func (d *Store) seedRelayEnrollmentTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, projectUID string) error {
	if grant.ProjectID == nil {
		return db.ErrNotFound
	}
	projectID := *grant.ProjectID
	compacted, err := relayHistoryNeedsResetTx(ctx, tx, projectID, projectUID)
	if err != nil {
		return err
	}
	if compacted {
		return nil
	}
	if err := d.seedRelayArtifactManifestsTx(ctx, tx, grant, projectUID); err != nil {
		return err
	}
	var after int64
	for {
		// #nosec G202 -- The event-type predicate is fixed native SQL; all project and cursor values remain bound.
		rows, err := tx.QueryContext(ctx, `SELECT e.id FROM events e WHERE e.project_id=$1 AND e.id>$2 AND `+pgFederationPushEventTypeCondition("e.type")+` ORDER BY e.id LIMIT 100`, projectID, after)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			event, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=$1`, id))
			if err != nil {
				return err
			}
			body, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(event))
			if err != nil {
				return err
			}
			if err = queueRelayBodyTx(ctx, tx, projectID, projectUID, db.RelayStreamEvent, event.UID, event.ContentHash, body, d.instanceUID, grant); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
	var epoch, sequence int64
	for {
		rows, err := tx.QueryContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=$1 AND (reset_epoch,sequence)>($2,$3) ORDER BY reset_epoch,sequence LIMIT 100`, projectUID, epoch, sequence)
		if err != nil {
			return err
		}
		var receipts []db.AttributionReceipt
		for rows.Next() {
			var receipt db.AttributionReceipt
			receipt, err = readReceipt(rows)
			if err != nil {
				break
			}
			receipts = append(receipts, receipt)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(receipts) == 0 {
			break
		}
		for _, receipt := range receipts {
			raw, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			if err = queueRelayBodyTx(ctx, tx, projectID, projectUID, db.RelayStreamReceipt, receipt.EventUID, receipt.ContentHash, raw, d.instanceUID, grant); err != nil {
				return err
			}
		}
		last := receipts[len(receipts)-1]
		epoch, sequence = last.ResetEpoch, last.Sequence
	}
	return nil
}

// seedRelayLocalEventsTx restores retained source-event delivery when an
// upstream relay binding is installed after standalone operation. The event
// rows and the binding share the caller's transaction so a reset cannot pass
// its pending-work checks between configuration and outbox seeding.
func (d *Store) seedRelayLocalEventsTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, projectUID string, afterID int64) error {
	if grant.ProjectID == nil {
		return db.ErrNotFound
	}
	projectID := *grant.ProjectID
	after := afterID
	for {
		// #nosec G202 -- The event-type predicate is fixed native SQL; project and cursor values remain bound.
		rows, err := tx.QueryContext(ctx, `SELECT e.id FROM events e WHERE e.project_id=$1 AND e.origin_instance_uid=$2 AND e.id>$3 AND `+pgFederationPushEventTypeCondition("e.type")+` ORDER BY e.id LIMIT 100`, projectID, d.instanceUID, after)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			event, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=$1`, id))
			if err != nil {
				return err
			}
			crossing, err := relayEventCrossesProjectTx(ctx, tx, projectID, db.RemoteEventFromStored(event))
			if err != nil {
				return err
			}
			if crossing {
				// Retain the original hashed event locally without replaying private issue data.
				continue
			}
			body, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(event))
			if err != nil {
				return err
			}
			if err := queueRelayBodyTx(ctx, tx, projectID, projectUID, db.RelayStreamEvent, event.UID, event.ContentHash, body, d.instanceUID, grant); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
}

// RelayEnrollmentNeedsReset detects incomplete history before offering a
// brand-new namespace. Current authority is checked under the native fence.
func (d *Store) RelayEnrollmentNeedsReset(ctx context.Context, bindingUID string) (bool, error) {
	var required bool
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "pull")
		if err != nil {
			return err
		}
		if grant.ID <= 0 {
			return db.ErrNotFound
		}
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, *grant.ProjectID).Scan(&projectUID); err != nil {
			return err
		}
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			return db.ErrNotFound
		}
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, db.RelayResetMetadataPrefix+projectUID+"."+bindingUID).Scan(&raw)
		if err == nil {
			var checkpoint db.RelayResetCheckpoint
			if err := json.Unmarshal([]byte(raw), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			required, err = d.relayCheckpointNeedsRefreshTx(ctx, tx, projectUID, checkpoint)
			required = required || checkpoint.Translation.Authority.Epoch != grant.RelayResetEpoch
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		required, err = d.relayHistoryNeedsResetCachedTx(ctx, tx, *grant.ProjectID, projectUID)
		return err
	})
	return required, err
}

func relayHistoryNeedsResetTx(ctx context.Context, tx *sql.Tx, projectID int64, projectUID string) (bool, error) {
	var required bool
	err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM purge_log WHERE project_id=$1 AND purge_reset_after_event_id IS NOT NULL)
 OR EXISTS(SELECT 1 FROM project_purge_log WHERE project_id=$1 AND purge_reset_after_event_id IS NOT NULL)
 OR EXISTS(SELECT 1 FROM issues i WHERE i.project_id=$1 AND NOT EXISTS(SELECT 1 FROM events e WHERE e.project_id=$1 AND e.issue_uid=i.uid AND e.type IN ('issue.created','issue.snapshot')))
 OR EXISTS(SELECT 1 FROM federation_entity_provenance ep WHERE ep.project_uid=$2 AND NOT EXISTS(SELECT 1 FROM events e WHERE e.uid=ep.event_uid AND e.project_id=$1))`, projectID, projectUID).Scan(&required)
	if err != nil || required {
		return required, err
	}
	return relayHistoryHasBoundaryTx(ctx, tx, projectID)
}

// Retained checkpoints are exact retry identities until a later root purge or
// an installed upstream checkpoint makes their snapshot obsolete.
func (d *Store) relayCheckpointNeedsRefreshTx(ctx context.Context, tx *sql.Tx, projectUID string, checkpoint db.RelayResetCheckpoint) (bool, error) {
	pin, err := rootPinTx(ctx, tx, projectUID, "")
	if err != nil {
		return false, err
	}
	if pin.AuthorityUID == d.instanceUID {
		// Pre-boundary checkpoints may retain now-excluded source records.
		// Rebuild once under the current selected-project snapshot rules.
		if checkpoint.Manifest.HistoryEventID == 0 {
			return true, nil
		}
		var latest int64
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(cursor),0) FROM (
   SELECT MAX(purge_reset_after_event_id) AS cursor FROM purge_log WHERE project_uid=$1
   UNION ALL SELECT MAX(purge_reset_after_event_id) AS cursor FROM project_purge_log WHERE project_uid=$1) AS boundaries`, projectUID).Scan(&latest)
		return latest > checkpoint.Manifest.HistoryEventID, err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, db.RelayResetMetadataPrefix+projectUID).Scan(&raw); err != nil {
		return false, err
	}
	var installed db.RelayResetCheckpoint
	if err := json.Unmarshal([]byte(raw), &installed, json.RejectUnknownMembers(true)); err != nil {
		return false, err
	}
	return installed.Manifest.SnapshotUID != checkpoint.Manifest.SnapshotUID, nil
}

// Seed complete durable manifests in bounded pages inside enrollment's transaction.
func (d *Store) seedRelayArtifactManifestsTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, projectUID string) error {
	if grant.ProjectID == nil {
		return db.ErrNotFound
	}
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT a.digest,a.manifest FROM federation_embedding_artifacts a JOIN issues i ON i.uid=a.issue_uid AND i.project_id=$1 WHERE a.project_uid=$2 AND a.digest>$3 AND a.staging_expires_at IS NULL AND i.deleted_at IS NULL ORDER BY a.digest LIMIT 32`, *grant.ProjectID, projectUID, after)
		if err != nil {
			return err
		}
		type retained struct{ digest, raw string }
		var page []retained
		for rows.Next() {
			var item retained
			if err = rows.Scan(&item.digest, &item.raw); err != nil {
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
			return nil
		}
		for _, item := range page {
			var manifest embedding.ArtifactManifest
			if json.Unmarshal([]byte(item.raw), &manifest, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifactManifest(manifest) != nil || manifest.ProjectUID != projectUID || manifest.Digest != item.digest {
				return db.ErrFederationIngestValidation
			}
			if err := queueRelayBodyTx(ctx, tx, *grant.ProjectID, projectUID, db.RelayStreamArtifact, item.digest, item.digest, []byte(item.raw), d.instanceUID, grant); err != nil {
				return err
			}
		}
		after = page[len(page)-1].digest
	}
}
