package db

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

// RootResetProvenance preserves creation references separately from snapshot
// author fields. The entire section is covered by the pinned root signature.
type RootResetProvenance struct {
	Keys     []RootKeyPin         `json:"keys"`
	Receipts []AttributionReceipt `json:"receipts"`
	Entities []EntityProvenance   `json:"entities"`
}

// DecodeRootResetPayload validates committed contents before native replacement.
// Event bodies retain the original payload bytes, including across compaction.
func DecodeRootResetPayload(pin RootKeyPin, snapshot RootResetSnapshot) ([]RemoteEvent, RootResetProvenance, error) {
	var sources, entities [][]byte
	var provenance RootResetProvenance
	if err := json.Unmarshal(snapshot.Events, &sources, json.RejectUnknownMembers(true)); err != nil {
		return nil, provenance, err
	}
	if err := json.Unmarshal(snapshot.Entities, &entities, json.RejectUnknownMembers(true)); err != nil {
		return nil, provenance, err
	}
	if err := json.Unmarshal(snapshot.Provenance, &provenance, json.RejectUnknownMembers(true)); err != nil {
		return nil, provenance, err
	}
	if sources == nil || entities == nil || provenance.Keys == nil || provenance.Receipts == nil || provenance.Entities == nil {
		return nil, provenance, errors.New("reset payload omits an authoritative collection")
	}
	var artifacts []embedding.ArtifactManifest
	if err := json.Unmarshal(snapshot.Artifacts, &artifacts, json.RejectUnknownMembers(true)); err != nil {
		return nil, provenance, err
	}
	if artifacts == nil {
		return nil, provenance, errors.New("reset payload omits artifact manifests")
	}
	keys := map[string]RootKeyPin{}
	active := 0
	for _, key := range provenance.Keys {
		if err := ValidateRootKeyPin(key); err != nil {
			return nil, provenance, err
		}
		if key.ProjectUID != pin.ProjectUID || key.AuthorityUID != pin.AuthorityUID {
			return nil, provenance, errors.New("reset key changes project authority")
		}
		if _, exists := keys[key.KeyID]; exists {
			return nil, provenance, errors.New("duplicate reset key")
		}
		keys[key.KeyID] = key
		if !key.Retired {
			active++
			if key.KeyID != pin.KeyID {
				return nil, provenance, errors.New("reset changes active root pin")
			}
		}
	}
	if active != 1 {
		return nil, provenance, errors.New("reset requires the existing active root pin")
	}
	events := make([]RemoteEvent, 0, len(sources)+len(entities))
	byUID := map[string]RemoteEvent{}
	for section, bodies := range [][][]byte{sources, entities} {
		for _, body := range bodies {
			event, err := DecodeRelaySourceEvent(body)
			if err != nil {
				return nil, provenance, err
			}
			if event.ProjectUID != pin.ProjectUID || !uid.Valid(event.EventUID) || !uid.Valid(event.OriginInstanceUID) {
				return nil, provenance, errors.New("invalid reset source identity or event type")
			}
			if section == 1 && (event.OriginInstanceUID != pin.AuthorityUID || (event.Type != "issue.snapshot" && event.Type != "project.metadata_updated")) {
				return nil, provenance, errors.New("reset entity section requires root snapshot records")
			}
			if err := ValidateFederationEntries(event.Type, event.EventUID, event.Payload); err != nil {
				return nil, provenance, err
			}
			if _, exists := byUID[event.EventUID]; exists {
				return nil, provenance, errors.New("duplicate reset event")
			}
			byUID[event.EventUID] = event
			events = append(events, event)
		}
	}
	receipts := map[string]AttributionReceipt{}
	sequences := map[string]bool{}
	for _, receipt := range provenance.Receipts {
		key, exists := keys[receipt.KeyID]
		if !exists {
			return nil, provenance, errors.New("reset receipt has no historical key")
		}
		if err := VerifyRootReceipt(key, receipt); err != nil {
			return nil, provenance, err
		}
		seq := fmt.Sprintf("%d/%d", receipt.ResetEpoch, receipt.Sequence)
		if _, exists := receipts[receipt.EventUID]; exists || sequences[seq] {
			return nil, provenance, errors.New("duplicate reset receipt")
		}
		receipts[receipt.EventUID] = receipt
		sequences[seq] = true
		if source, exists := byUID[receipt.EventUID]; exists {
			handle, _, err := EventCreationProvenance(source)
			if err != nil {
				return nil, provenance, err
			}
			if source.ContentHash != receipt.ContentHash || source.Actor != receipt.SourceActor || handle != receipt.Teammate {
				return nil, provenance, ErrRemoteEventHashMismatch
			}
		}
	}
	projection := FoldEvents(remoteResetFoldEvents(events))
	digests := map[string]bool{}
	for _, artifact := range artifacts {
		_, issueExists := projection.Issues[artifact.IssueUID]
		if embedding.ValidateArtifactManifest(artifact) != nil || artifact.ProjectUID != pin.ProjectUID || !issueExists || digests[artifact.Digest] {
			return nil, provenance, errors.New("invalid reset artifact manifest")
		}
		digests[artifact.Digest] = true
	}
	refs := map[string]bool{}
	for _, ref := range provenance.Entities {
		identity := ref.Kind + "/" + ref.EntityUID
		_, receiptExists := receipts[ref.EventUID]
		_, issueExists := projection.Issues[ref.EntityUID]
		_, commentExists := projection.Comments[ref.EntityUID]
		if ref.ProjectUID != pin.ProjectUID || !uid.Valid(ref.EntityUID) || !receiptExists || refs[identity] || (ref.Kind != "issue" && ref.Kind != "comment") || (ref.Kind == "issue" && !issueExists) || (ref.Kind == "comment" && !commentExists) {
			return nil, provenance, errors.New("invalid reset creation reference")
		}
		if source, exists := byUID[ref.EventUID]; exists {
			_, creation, err := EventCreationProvenance(source)
			if err != nil {
				return nil, provenance, err
			}
			matches := false
			for _, candidate := range creation {
				if candidate == ref {
					matches = true
				}
			}
			if !matches {
				return nil, provenance, errors.New("reset reference disagrees with original creation source")
			}
		}
		refs[identity] = true
	}
	return events, provenance, nil
}

func remoteResetFoldEvents(events []RemoteEvent) []FoldEvent {
	result := make([]FoldEvent, 0, len(events))
	for _, event := range events {
		fold := FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(EventTimestampFormat), Payload: event.Payload}
		if event.IssueUID != nil {
			fold.IssueUID = *event.IssueUID
		}
		if event.RelatedIssueUID != nil {
			fold.RelatedIssueUID = *event.RelatedIssueUID
		}
		result = append(result, fold)
	}
	return result
}

// RelayResetMetadataPrefix scopes signed reset records in daemon-local metadata.
const RelayResetMetadataPrefix = "relay_reset."

// RelayResetCheckpoint retains exact retry and forwarding bytes. The root
// manifest is unchanged; translation describes only this daemon's upstream hop.
type RelayResetCheckpoint struct {
	Manifest    RootResetManifest     `json:"manifest"`
	Snapshot    RootResetSnapshot     `json:"snapshot"`
	Translation RelayResetTranslation `json:"translation"`
}
