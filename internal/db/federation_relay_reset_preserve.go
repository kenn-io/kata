package db

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// RelayRetainedReceiptRow is one stored root receipt joined to the event row
// it attests, as the backend loads it: the raw receipt plus the columns the
// store indexed from it.
type RelayRetainedReceiptRow struct {
	EventID     int64
	Receipt     string
	ContentHash string
	ResetEpoch  int64
	Sequence    int64
	KeyID       string
}

// RelayPostCheckpointCandidate is a retained root receipt newer than the
// checkpoint being installed and the stored event it attests.
type RelayPostCheckpointCandidate struct {
	EventID int64
	Receipt AttributionReceipt
}

// RelayPostCheckpointHighWater returns the greatest receipt position in a
// checkpoint's provenance. Root reset provenance contains every receipt known
// at capture, so that position is the checkpoint's project-wide high-water
// mark.
func RelayPostCheckpointHighWater(receipts []AttributionReceipt) (int64, int64) {
	var epoch, sequence int64
	for _, receipt := range receipts {
		if receipt.ResetEpoch > epoch || (receipt.ResetEpoch == epoch && receipt.Sequence > sequence) {
			epoch, sequence = receipt.ResetEpoch, receipt.Sequence
		}
	}
	return epoch, sequence
}

// RelayPostCheckpointCandidates decodes retained receipt rows and keeps the
// ones a checkpoint install must replay: receipts past the checkpoint's
// high-water mark (epoch, sequence) for events the checkpoint does not
// already carry in known. A row whose indexed columns disagree with its
// stored receipt is corrupt and fails with ErrFederationIngestValidation.
func RelayPostCheckpointCandidates(rows []RelayRetainedReceiptRow, projectUID string, known map[string]struct{}, epoch, sequence int64) ([]RelayPostCheckpointCandidate, error) {
	var candidates []RelayPostCheckpointCandidate
	for _, row := range rows {
		var receipt AttributionReceipt
		if err := json.Unmarshal([]byte(row.Receipt), &receipt, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		if receipt.ProjectUID != projectUID || receipt.ContentHash != row.ContentHash || receipt.ResetEpoch != row.ResetEpoch || receipt.Sequence != row.Sequence || receipt.KeyID != row.KeyID {
			return nil, fmt.Errorf("%w: retained root receipt metadata differs", ErrFederationIngestValidation)
		}
		if _, exists := known[receipt.EventUID]; exists {
			continue
		}
		if receipt.ResetEpoch < epoch || (receipt.ResetEpoch == epoch && receipt.Sequence <= sequence) {
			continue
		}
		candidates = append(candidates, RelayPostCheckpointCandidate{EventID: row.EventID, Receipt: receipt})
	}
	return candidates, nil
}

// RetainPostCheckpointEvents keeps root-accepted events that arrived after
// the signed checkpoint was captured. They are replayed after its exact
// snapshot so installing an older forwarded checkpoint cannot erase accepted
// local intent. The events, receipts, keys and creation references they need
// are appended to events and provenance.
func (p RelayResetProtocol) RetainPostCheckpointEvents(ctx context.Context, projectID int64, projectUID string, checkpointPin RootKeyPin, events []RemoteEvent, provenance *RootResetProvenance) ([]RemoteEvent, error) {
	knownEvents := make(map[string]struct{}, len(events))
	for _, event := range events {
		knownEvents[event.EventUID] = struct{}{}
	}
	knownReceipts := make(map[string]struct{}, len(provenance.Receipts))
	for _, receipt := range provenance.Receipts {
		knownReceipts[receipt.EventUID] = struct{}{}
	}
	knownKeys := make(map[string]struct{}, len(provenance.Keys))
	for _, key := range provenance.Keys {
		knownKeys[key.KeyID] = struct{}{}
	}
	knownEntities := make(map[string]struct{}, len(provenance.Entities))
	for _, ref := range provenance.Entities {
		knownEntities[ref.Kind+"/"+ref.EntityUID] = struct{}{}
	}
	checkpointEpoch, checkpointSequence := RelayPostCheckpointHighWater(provenance.Receipts)
	rows, err := p.Backend.RelayPostCheckpointReceipts(ctx, projectID, projectUID, checkpointEpoch, checkpointSequence)
	if err != nil {
		return nil, err
	}
	candidates, err := RelayPostCheckpointCandidates(rows, projectUID, knownReceipts, checkpointEpoch, checkpointSequence)
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		receipt := candidate.Receipt
		rootKey, err := p.Backend.RootPin(ctx, projectUID, receipt.KeyID)
		if err != nil {
			return nil, err
		}
		if rootKey.AuthorityUID != checkpointPin.AuthorityUID {
			return nil, errors.New("retained root receipt changes checkpoint authority")
		}
		if err := VerifyRootReceipt(rootKey, receipt); err != nil {
			return nil, err
		}
		stored, err := p.Backend.StoredEvent(ctx, candidate.EventID)
		if err != nil {
			return nil, err
		}
		source := RemoteEventFromStored(stored)
		if source.ProjectUID != projectUID || source.EventUID != receipt.EventUID || source.ContentHash != receipt.ContentHash || source.Actor != receipt.SourceActor {
			return nil, ErrRemoteEventHashMismatch
		}
		if _, err := EncodeRelaySourceEvent(source); err != nil {
			return nil, err
		}
		handle, refs, err := EventCreationProvenance(source)
		if err != nil {
			return nil, err
		}
		if handle != receipt.Teammate {
			return nil, ErrRemoteEventHashMismatch
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
