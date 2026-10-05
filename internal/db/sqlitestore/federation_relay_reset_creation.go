package sqlitestore

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

// CreateRelayReset captures a root's current snapshot and creation proofs under
// its existing native transaction and signing authority. Exact retries disclose
// the retained checkpoint only while the requesting enrollment remains live.
func (d *Store) CreateRelayReset(ctx context.Context, bindingUID string, signer db.RootAttributionSigner) (db.RelayResetCheckpoint, error) {
	var result db.RelayResetCheckpoint
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.RelayResetCheckpoint{}
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "pull")
		if err != nil {
			return err
		}
		if grant.ID <= 0 {
			return errors.New("root reset generation requires a downstream enrollment")
		}
		projectID := *grant.ProjectID
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=?`, projectID))
		if err != nil {
			return err
		}
		if !db.ProjectAttributionVisible(ctx, project.UID) {
			return db.ErrNotFound
		}
		pin, err := rootPinTx(ctx, tx, project.UID, "")
		if err != nil {
			return err
		}
		if pin.AuthorityUID == d.instanceUID && (signer.AuthorityUID != d.instanceUID || len(signer.PrivateKey) != ed25519.PrivateKeySize || db.RootPublicKeyID(signer.PrivateKey.Public().(ed25519.PublicKey)) != pin.KeyID) {
			return errors.New("root reset signing authority does not match pinned owner key")
		}
		key := db.RelayResetMetadataPrefix + project.UID + "." + bindingUID
		var cached string
		err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&cached)
		if err == nil {
			if err := json.Unmarshal([]byte(cached), &result, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			authority := db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: d.instanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: result.Translation.Authority.Epoch}
			if result.Translation.Authority.Epoch != grant.RelayResetEpoch && result.Translation.Authority.Epoch != grant.RelayResetEpoch+1 {
				return db.ErrFederationIngestValidation
			}
			if err := db.ValidateRelayResetTranslation(authority, result.Manifest, result.Translation); err != nil {
				return err
			}
			historical, err := rootPinTx(ctx, tx, project.UID, result.Manifest.KeyID)
			if err != nil {
				return err
			}
			if historical.AuthorityUID != pin.AuthorityUID {
				return errors.New("cached reset changes pinned root authority")
			}
			if err := db.VerifyRootResetManifest(historical, result.Manifest, result.Snapshot); err != nil {
				return err
			}
			refresh, err := d.relayCheckpointNeedsRefreshTx(ctx, tx, project.UID, result)
			if err != nil {
				return err
			}

			if !refresh && result.Translation.Authority.Epoch == grant.RelayResetEpoch+1 {
				var latest int64
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=?`, projectID).Scan(&latest); err != nil {
					return err
				}
				refresh = latest > result.Manifest.HistoryEventID
			}
			if !refresh {
				_, err = d.relayGrantTx(ctx, tx, bindingUID, "pull")
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox WHERE binding_uid=? AND reset_epoch=? AND acknowledged=0)`, bindingUID, grant.RelayResetEpoch).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return db.ErrFederationResetBlockedByPendingPush
		}
		if pin.AuthorityUID != d.instanceUID {
			result, err = d.forwardRelayResetTx(ctx, tx, grant, pin)
			return err
		}
		// Existing baseline events advance the same durable HLC as later root writes.
		// Snapshot labels remain state; only the original signed creation references
		// below confer creator verification. Captured records do not create echo work.
		boundary, err := d.insertFederationBaselineEventsTx(db.WithRelayStateCapture(ctx), tx, project, grant.Actor)
		if err != nil {
			return err
		}
		payload, err := db.ProjectMetadataAdoptionPayload(project.Metadata)
		if err != nil {
			return err
		}
		clock := db.EventHLCTimestamp{PhysicalMS: boundary.HLCPhysicalMS, Counter: boundary.HLCCounter}
		_, err = d.insertEventTx(db.WithRelayStateCapture(ctx), tx, eventInsert{ProjectID: projectID, ProjectUID: project.UID, ProjectName: project.Name, Type: "project.metadata_updated", Actor: grant.Actor, Payload: payload, HLC: &clock, CreatedAt: boundary.CreatedAt.UTC().Format(db.EventTimestampFormat)})
		if err != nil {
			return err
		}
		// Read the existing portable history once. The new baseline's current entity
		// records are committed separately from immutable source history.
		// #nosec G202 -- The event-type predicate is fixed native SQL; all project and cursor values remain bound.
		rows, err := tx.QueryContext(ctx, `SELECT id FROM events WHERE project_id=? AND `+federationPushEventTypeCondition("type")+` ORDER BY id`, projectID)
		if err != nil {
			return err
		}
		ids := []int64{}
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
		sources, entities := [][]byte{}, [][]byte{}
		for _, id := range ids {
			event, err := scanEvent(tx.QueryRowContext(ctx, eventSelectByID, id))
			if err != nil {
				return err
			}
			if id < boundary.ID {
				crossing, err := relayEventCrossesProjectTx(ctx, tx, projectID, db.RemoteEventFromStored(event))
				if err != nil {
					return err
				}
				if crossing {
					// Keep original bytes owner-local; never rewrite a source hash
					// to remove a private historical endpoint from this checkpoint.
					continue
				}
			}
			raw, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(event))
			if err != nil {
				return err
			}
			if id >= boundary.ID {
				entities = append(entities, raw)
			} else {
				sources = append(sources, raw)
			}
		}
		provenance := db.RootResetProvenance{Keys: []db.RootKeyPin{}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{}}
		rows, err = tx.QueryContext(ctx, `SELECT project_uid,authority_uid,key_id,public_key,active FROM federation_root_keys WHERE project_uid=? ORDER BY key_id`, project.UID)
		if err != nil {
			return err
		}
		for rows.Next() {
			pin, readErr := scanRootPin(rows)
			if readErr != nil {
				err = readErr
				break
			}
			provenance.Keys = append(provenance.Keys, pin)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=? ORDER BY reset_epoch,sequence`, project.UID)
		if err != nil {
			return err
		}
		for rows.Next() {
			receipt, readErr := readReceipt(rows)
			if readErr != nil {
				err = readErr
				break
			}
			provenance.Receipts = append(provenance.Receipts, receipt)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT ep.project_uid,ep.kind,ep.entity_uid,ep.event_uid FROM federation_entity_provenance ep
WHERE ep.project_uid=? AND (
 (ep.kind='issue' AND EXISTS(SELECT 1 FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=ep.project_uid AND i.uid=ep.entity_uid)) OR
 (ep.kind='comment' AND EXISTS(SELECT 1 FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE p.uid=ep.project_uid AND c.uid=ep.entity_uid)))
ORDER BY ep.kind,ep.entity_uid`, project.UID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ref db.EntityProvenance
			if err = rows.Scan(&ref.ProjectUID, &ref.Kind, &ref.EntityUID, &ref.EventUID); err != nil {
				break
			}
			provenance.Entities = append(provenance.Entities, ref)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		// The signed snapshot commits exact complete chunk manifests. Bytes remain
		// on the independent manifest-first artifact stream, never in reset ACKs.
		artifacts := []embedding.ArtifactManifest{}
		rows, err = tx.QueryContext(ctx, `SELECT a.digest,a.manifest FROM federation_embedding_artifacts a JOIN issues i ON i.uid=a.issue_uid AND i.project_id=? WHERE a.project_uid=? AND a.staging_expires_at IS NULL AND i.deleted_at IS NULL ORDER BY a.digest`, project.ID, project.UID)
		if err != nil {
			return err
		}
		manifestBytes := 2
		for rows.Next() {
			var digest, raw string
			if err = rows.Scan(&digest, &raw); err != nil {
				break
			}
			manifestBytes += len(raw) + 1
			if manifestBytes > db.MaxRelayEnvelopeBytes {
				err = errors.New("root reset exceeds artifact manifest byte limit")
				break
			}
			var manifest embedding.ArtifactManifest
			if json.Unmarshal([]byte(raw), &manifest, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifactManifest(manifest) != nil || manifest.ProjectUID != project.UID || manifest.Digest != digest {
				err = db.ErrFederationIngestValidation
				break
			}
			artifacts = append(artifacts, manifest)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		snapshot := db.RootResetSnapshot{}
		snapshot.Artifacts, err = json.Marshal(artifacts)
		if err != nil {
			return err
		}
		snapshot.Events, err = json.Marshal(sources)
		if err != nil {
			return err
		}
		snapshot.Entities, err = json.Marshal(entities)
		if err != nil {
			return err
		}
		snapshot.Provenance, err = json.Marshal(provenance)
		if err != nil {
			return err
		}
		if len(snapshot.Events)+len(snapshot.Entities)+len(snapshot.Provenance)+len(snapshot.Artifacts) > db.MaxRelayEnvelopeBytes {
			return errors.New("root reset exceeds snapshot byte limit")
		}
		if _, _, err := db.DecodeRootResetPayload(pin, snapshot); err != nil {
			return err
		}
		// Root epochs are per project, independent of every binding's hop epoch.
		// Compare all retained root checkpoints so a second enrollment cannot reuse
		// an epoch with a different signed snapshot.
		prefix := db.RelayResetMetadataPrefix + project.UID + "."
		rows, err = tx.QueryContext(ctx, `SELECT value FROM meta WHERE substr(key,1,length(?))=?`, prefix, prefix)
		if err != nil {
			return err
		}
		rootEpoch := int64(1)
		for rows.Next() {
			var value string
			if err = rows.Scan(&value); err != nil {
				break
			}
			var checkpoint db.RelayResetCheckpoint
			if err = json.Unmarshal([]byte(value), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
				break
			}
			rootEpoch = max(rootEpoch, checkpoint.Manifest.ResetEpoch)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		rootEpoch++
		snapshotUID, err := uid.New()
		if err != nil {
			return err
		}
		var baselines db.RelayStreamCursors
		for stream, target := range map[string]*int64{db.RelayStreamEvent: &baselines.Events, db.RelayStreamReceipt: &baselines.Receipts, db.RelayStreamArtifact: &baselines.Artifacts} {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM federation_relay_outbox WHERE project_uid=? AND stream=?`, project.UID, stream).Scan(target); err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=?`, projectID).Scan(&boundary.ID); err != nil {
			return err
		}
		manifest, err := db.SignRootResetManifest(db.RootResetManifest{Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, ResetEpoch: rootEpoch, SnapshotUID: snapshotUID, RootBaselines: baselines, HistoryEventID: boundary.ID}, snapshot, signer.PrivateKey)
		if err != nil {
			return err
		}
		newEpoch := grant.RelayResetEpoch + 1
		translation := db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: d.instanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: newEpoch}, SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest}
		result = db.RelayResetCheckpoint{Manifest: manifest, Snapshot: snapshot, Translation: translation}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(raw)); err != nil {
			return err
		}
		grant.RelayResetEpoch = newEpoch
		if err := d.seedRelayArtifactManifestsTx(ctx, tx, grant, project.UID); err != nil {
			return err
		}
		_, err = d.relayGrantTx(ctx, tx, bindingUID, "pull")
		return err
	})
	if err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	return result, nil
}
