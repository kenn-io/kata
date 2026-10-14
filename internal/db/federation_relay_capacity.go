package db

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/embedding"
)

// RelayCheckpointCapacityBytes is the largest signed reset checkpoint a root
// builds for one project: the serialized history, current-state records,
// creation proofs and artifact manifests together. Ordinary sync moves
// bounded batches and has no project-size limit; recovery after a purge or
// for a new enrollment needs the whole project in one checkpoint, so a root
// refuses to let a project grow past this capacity through relay enrollment.
const RelayCheckpointCapacityBytes = MaxRelayEnvelopeBytes

// RelayCheckpointCapacity returns the configured capacity, or the default when
// override is not positive. Stores pass their test override.
func RelayCheckpointCapacity(override int64) int64 {
	if override > 0 {
		return override
	}
	return RelayCheckpointCapacityBytes
}

// RelayProjectTooLargeError reports a project whose signed checkpoint would
// exceed the relay checkpoint capacity.
type RelayProjectTooLargeError struct {
	ProjectUID string
	Bytes      int64
	Capacity   int64
}

func (e *RelayProjectTooLargeError) Error() string {
	return fmt.Sprintf("relay project %s checkpoint is %d bytes; the capacity is %d bytes", e.ProjectUID, e.Bytes, e.Capacity)
}

// RelayCheckpointSize is the measured checkpoint size of one project against
// the capacity that applies to it.
type RelayCheckpointSize struct {
	Bytes    int64
	Capacity int64
}

// CapacityError returns the refusal for a size above capacity, or nil.
func (s RelayCheckpointSize) CapacityError(projectUID string) error {
	if s.Bytes > s.Capacity {
		return &RelayProjectTooLargeError{ProjectUID: projectUID, Bytes: s.Bytes, Capacity: s.Capacity}
	}
	return nil
}

// RelayCheckpointMeasurer reports the checkpoint size a root would build for a
// project right now, without creating a checkpoint.
type RelayCheckpointMeasurer interface {
	RelayCheckpointSize(ctx context.Context, projectID int64) (RelayCheckpointSize, error)
}

// RelayCheckpointRecords are the loaded records one signed checkpoint
// serializes: retained portable history, current-state records, creation
// proofs and complete artifact manifests.
type RelayCheckpointRecords struct {
	Sources    [][]byte
	Entities   [][]byte
	Provenance RootResetProvenance
	Artifacts  []embedding.ArtifactManifest
}

// RelayCheckpointIssue is one issue's current state as the source of a
// checkpoint record: the issue UID and its federation snapshot payload.
type RelayCheckpointIssue struct {
	IssueUID string
	Payload  string
}

// AppendEvents encodes loaded portable events in id order. Events below
// boundaryID are retained history and are skipped when crosses reports that
// they reference another project; events at or above it are the current-state
// records a checkpoint creation just wrote.
func (r *RelayCheckpointRecords) AppendEvents(events []Event, boundaryID int64, crosses func(RemoteEvent) (bool, error)) error {
	for _, event := range events {
		remote := RemoteEventFromStored(event)
		if event.ID < boundaryID {
			crossing, err := crosses(remote)
			if err != nil {
				return err
			}
			if crossing {
				// Keep original bytes owner-local; never rewrite a source hash
				// to remove a private historical endpoint from this checkpoint.
				continue
			}
		}
		raw, err := EncodeRelaySourceEvent(remote)
		if err != nil {
			return err
		}
		if event.ID >= boundaryID {
			r.Entities = append(r.Entities, raw)
		} else {
			r.Sources = append(r.Sources, raw)
		}
	}
	return nil
}

// Snapshot serializes the records into the committed snapshot sections. Empty
// sections are explicit, as a signed checkpoint requires.
func (r RelayCheckpointRecords) Snapshot() (RootResetSnapshot, error) {
	if r.Sources == nil {
		r.Sources = [][]byte{}
	}
	if r.Entities == nil {
		r.Entities = [][]byte{}
	}
	if r.Artifacts == nil {
		r.Artifacts = []embedding.ArtifactManifest{}
	}
	if r.Provenance.Keys == nil {
		r.Provenance.Keys = []RootKeyPin{}
	}
	if r.Provenance.Receipts == nil {
		r.Provenance.Receipts = []AttributionReceipt{}
	}
	if r.Provenance.Entities == nil {
		r.Provenance.Entities = []EntityProvenance{}
	}
	var snapshot RootResetSnapshot
	var err error
	if snapshot.Artifacts, err = json.Marshal(r.Artifacts); err != nil {
		return RootResetSnapshot{}, fmt.Errorf("marshal checkpoint artifacts: %w", err)
	}
	if snapshot.Events, err = json.Marshal(r.Sources); err != nil {
		return RootResetSnapshot{}, fmt.Errorf("marshal checkpoint events: %w", err)
	}
	if snapshot.Entities, err = json.Marshal(r.Entities); err != nil {
		return RootResetSnapshot{}, fmt.Errorf("marshal checkpoint entities: %w", err)
	}
	if snapshot.Provenance, err = json.Marshal(r.Provenance); err != nil {
		return RootResetSnapshot{}, fmt.Errorf("marshal checkpoint provenance: %w", err)
	}
	return snapshot, nil
}

// RootResetSnapshotBytes is the size a checkpoint counts against the capacity.
func RootResetSnapshotBytes(snapshot RootResetSnapshot) int64 {
	return int64(len(snapshot.Events) + len(snapshot.Entities) + len(snapshot.Provenance) + len(snapshot.Artifacts))
}

// Representative identity for a measured current-state record and its receipt.
// Checkpoint creation writes fresh event UIDs, hashes, clocks and sequence
// numbers; measurement needs values at least as long as the real ones, and the
// same values on every call.
const (
	relayCheckpointRecordUID   = "00000000000000000000000000"
	relayCheckpointRecordHash  = "0000000000000000000000000000000000000000000000000000000000000000"
	relayCheckpointRecordMS    = 1_700_000_000_001
	relayCheckpointRecordBound = 999_999_999
)

// RelayCheckpointArtifact validates one retained manifest row and reports
// whether it belongs in a checkpoint: a complete manifest for the issue's
// current content. Manifests for older inputs cannot index current content.
func RelayCheckpointArtifact(projectUID, digest, raw, title, body string) (embedding.ArtifactManifest, bool, error) {
	var manifest embedding.ArtifactManifest
	if json.Unmarshal([]byte(raw), &manifest, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifactManifest(manifest) != nil || manifest.ProjectUID != projectUID || manifest.Digest != digest {
		return embedding.ArtifactManifest{}, false, fmt.Errorf("%w: retained artifact manifest %s is malformed", ErrFederationIngestValidation, digest)
	}
	return manifest, manifest.InputHash == embedding.ArtifactInputHash(embedding.EmbedText(title, body)), nil
}

// RelayCheckpointBaseline is what a checkpoint capture adds to a project
// beyond its retained history: the current-state records it writes and the
// receipts a root-attributed capture signs for them.
type RelayCheckpointBaseline struct {
	Entities [][]byte
	Receipts []AttributionReceipt
}

// RelayCheckpointBaselineRecords builds the baseline a checkpoint capture would
// write without inserting events: the project metadata record, one snapshot
// per issue and the cron definition snapshots, each with the receipt a
// root-attributed capture signs for it, plus the receipt of the federation
// enable marker that capture writes but history does not carry. actor is the
// account the capture writes with; a longer actor only enlarges the estimate.
func RelayCheckpointBaselineRecords(project Project, instanceUID, actor string, issues []RelayCheckpointIssue, cron []CronEvent) (RelayCheckpointBaseline, error) {
	metadata, err := ProjectMetadataAdoptionPayload(project.Metadata)
	if err != nil {
		return RelayCheckpointBaseline{}, err
	}
	record, err := relayCheckpointEntityRecord(project, instanceUID, actor, "project.metadata_updated", nil, metadata)
	if err != nil {
		return RelayCheckpointBaseline{}, err
	}
	baseline := RelayCheckpointBaseline{Entities: [][]byte{record}}
	for _, issue := range issues {
		issueUID := issue.IssueUID
		record, err := relayCheckpointEntityRecord(project, instanceUID, actor, "issue.snapshot", &issueUID, issue.Payload)
		if err != nil {
			return RelayCheckpointBaseline{}, err
		}
		baseline.Entities = append(baseline.Entities, record)
	}
	for _, event := range cron {
		record, err := relayCheckpointEntityRecord(project, instanceUID, actor, event.Type, event.IssueUID, event.Payload)
		if err != nil {
			return RelayCheckpointBaseline{}, err
		}
		baseline.Entities = append(baseline.Entities, record)
	}
	for range len(baseline.Entities) + 1 {
		baseline.Receipts = append(baseline.Receipts, relayCheckpointReceipt(project.UID, instanceUID, actor))
	}
	return baseline, nil
}

// RelayCheckpointActorBound returns the actor whose JSON encoding is largest,
// so records and receipts measured with it are at least as large as those a
// capture writes for any of the given accounts. Character counts are not a
// bound: multibyte and escaped characters encode to several bytes each.
func RelayCheckpointActorBound(actors []string) (string, error) {
	bound, size := "", 0
	for _, actor := range actors {
		raw, err := json.Marshal(actor)
		if err != nil {
			return "", fmt.Errorf("encode relay actor: %w", err)
		}
		if len(raw) > size {
			bound, size = actor, len(raw)
		}
	}
	return bound, nil
}

// relayCheckpointReceipt is the receipt a root-attributed capture signs for
// one record it writes, with every variable field at its longest encoding:
// a full-precision acceptance time, nine-digit epoch and sequence numbers and
// a signature of the real length.
func relayCheckpointReceipt(projectUID, instanceUID, actor string) AttributionReceipt {
	return AttributionReceipt{
		Version:            1,
		ProjectUID:         projectUID,
		EventUID:           relayCheckpointRecordUID,
		ContentHash:        relayCheckpointRecordHash,
		AuthorityUID:       instanceUID,
		AccountableActor:   actor,
		SourceActor:        actor,
		IngressInstanceUID: instanceUID,
		AcceptedAt:         time.Unix(relayCheckpointRecordMS/1000, 123_456_789).UTC(),
		ResetEpoch:         relayCheckpointRecordBound,
		Sequence:           relayCheckpointRecordBound,
		KeyID:              relayCheckpointRecordHash,
		Signature:          make([]byte, ed25519.SignatureSize),
	}
}

// relayCheckpointEntityRecord encodes the current-state record that checkpoint
// creation would write for payload. Identity fields take representative values
// no shorter than the real ones, so the encoded size bounds the real record.
func relayCheckpointEntityRecord(project Project, instanceUID, actor, eventType string, issueUID *string, payload string) ([]byte, error) {
	now := time.UnixMilli(relayCheckpointRecordMS).UTC()
	event := RemoteEvent{
		EventUID:          relayCheckpointRecordUID,
		OriginInstanceUID: instanceUID,
		ProjectUID:        project.UID,
		ProjectName:       project.Name,
		IssueUID:          issueUID,
		Type:              eventType,
		Actor:             actor,
		HLCPhysicalMS:     now.UnixMilli(),
		HLCCounter:        relayCheckpointRecordBound,
		Payload:           []byte(payload),
		CreatedAt:         now,
	}
	hash, err := RemoteEventContentHash(event)
	if err != nil {
		return nil, err
	}
	event.ContentHash = hash
	return EncodeRelaySourceEvent(event)
}
