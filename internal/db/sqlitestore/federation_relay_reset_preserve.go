package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

type postCheckpointRelayEvent struct {
	id      int64
	receipt db.AttributionReceipt
}

// retainPostCheckpointRelayEventsTx keeps root-accepted events that arrived
// after the signed checkpoint was captured. They are replayed after its exact
// snapshot so installing an older forwarded checkpoint cannot erase accepted
// local intent.
func (d *Store) retainPostCheckpointRelayEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	projectUID string,
	checkpointPin db.RootKeyPin,
	events []db.RemoteEvent,
	provenance *db.RootResetProvenance,
) ([]db.RemoteEvent, error) {
	knownEvents := make(map[string]struct{}, len(events))
	for _, event := range events {
		knownEvents[event.EventUID] = struct{}{}
	}
	knownReceipts := make(map[string]struct{}, len(provenance.Receipts))
	var checkpointEpoch, checkpointSequence int64
	for _, receipt := range provenance.Receipts {
		knownReceipts[receipt.EventUID] = struct{}{}
		if receipt.ResetEpoch > checkpointEpoch || (receipt.ResetEpoch == checkpointEpoch && receipt.Sequence > checkpointSequence) {
			checkpointEpoch, checkpointSequence = receipt.ResetEpoch, receipt.Sequence
		}
	}
	knownKeys := make(map[string]struct{}, len(provenance.Keys))
	for _, key := range provenance.Keys {
		knownKeys[key.KeyID] = struct{}{}
	}
	knownEntities := make(map[string]struct{}, len(provenance.Entities))
	for _, ref := range provenance.Entities {
		knownEntities[ref.Kind+"/"+ref.EntityUID] = struct{}{}
	}

	// Root reset provenance contains every receipt known at checkpoint capture,
	// so the greatest receipt position is its project-wide high-water mark.
	rows, err := tx.QueryContext(ctx, `SELECT e.id,p.receipt,p.content_hash,p.reset_epoch,p.sequence,p.key_id
FROM events e JOIN federation_event_provenance p ON p.project_uid=? AND p.event_uid=e.uid
WHERE e.project_id=? AND (p.reset_epoch>? OR (p.reset_epoch=? AND p.sequence>?))
ORDER BY p.reset_epoch,p.sequence,e.uid`, projectUID, projectID, checkpointEpoch, checkpointEpoch, checkpointSequence)
	if err != nil {
		return nil, err
	}
	var candidates []postCheckpointRelayEvent
	for rows.Next() {
		var candidate postCheckpointRelayEvent
		var raw, contentHash, keyID string
		var epoch, sequence int64
		if err := rows.Scan(&candidate.id, &raw, &contentHash, &epoch, &sequence, &keyID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &candidate.receipt, json.RejectUnknownMembers(true)); err != nil {
			_ = rows.Close()
			return nil, err
		}
		receipt := candidate.receipt
		if receipt.ProjectUID != projectUID || receipt.ContentHash != contentHash || receipt.ResetEpoch != epoch || receipt.Sequence != sequence || receipt.KeyID != keyID {
			_ = rows.Close()
			return nil, fmt.Errorf("%w: retained root receipt metadata differs", db.ErrFederationIngestValidation)
		}
		if _, exists := knownReceipts[receipt.EventUID]; exists {
			continue
		}
		if receipt.ResetEpoch < checkpointEpoch || (receipt.ResetEpoch == checkpointEpoch && receipt.Sequence <= checkpointSequence) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	for _, candidate := range candidates {
		receipt := candidate.receipt
		rootKey, err := rootPinTx(ctx, tx, projectUID, receipt.KeyID)
		if err != nil {
			return nil, err
		}
		if rootKey.AuthorityUID != checkpointPin.AuthorityUID {
			return nil, errors.New("retained root receipt changes checkpoint authority")
		}
		if err := db.VerifyRootReceipt(rootKey, receipt); err != nil {
			return nil, err
		}
		stored, err := scanEvent(tx.QueryRowContext(ctx, eventSelectByID, candidate.id))
		if err != nil {
			return nil, err
		}
		source := db.RemoteEventFromStored(stored)
		if source.ProjectUID != projectUID || source.EventUID != receipt.EventUID || source.ContentHash != receipt.ContentHash || source.Actor != receipt.SourceActor {
			return nil, db.ErrRemoteEventHashMismatch
		}
		if _, err := db.EncodeRelaySourceEvent(source); err != nil {
			return nil, err
		}
		handle, refs, err := db.EventCreationProvenance(source)
		if err != nil {
			return nil, err
		}
		if handle != receipt.Teammate {
			return nil, db.ErrRemoteEventHashMismatch
		}
		if _, exists := knownEvents[source.EventUID]; !exists {
			events = append(events, source)
			knownEvents[source.EventUID] = struct{}{}
		}
		provenance.Receipts = append(provenance.Receipts, receipt)
		knownReceipts[receipt.EventUID] = struct{}{}
		if _, exists := knownKeys[rootKey.KeyID]; !exists {
			rootKey.Retired = true
			provenance.Keys = append(provenance.Keys, rootKey)
			knownKeys[rootKey.KeyID] = struct{}{}
		}
		for _, ref := range refs {
			identity := ref.Kind + "/" + ref.EntityUID
			if _, exists := knownEntities[identity]; exists {
				continue
			}
			provenance.Entities = append(provenance.Entities, ref)
			knownEntities[identity] = struct{}{}
		}
	}
	return events, nil
}
