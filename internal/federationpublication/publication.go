// Package federationpublication implements the closed portable relay publication
// codec and snapshot capture without depending on daemon or JSONL tooling.
package federationpublication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"strings"

	"go.kenn.io/kata/internal/db"
)

type recordEnvelope struct {
	Kind string         `json:"kind"`
	Data jsontext.Value `json:"data"`
}

type snapshotter interface {
	BeginExportSnapshot(context.Context) (db.Storage, func() error, error)
}

type publicationWire struct {
	Version           int              `json:"version"`
	OperationUID      string           `json:"operation_uid"`
	SourceInstanceUID string           `json:"source_instance_uid"`
	ProjectUID        string           `json:"project_uid"`
	TargetInstanceUID string           `json:"target_instance_uid"`
	ProjectName       string           `json:"project_name"`
	Digest            string           `json:"digest,omitzero"`
	Records           []recordEnvelope `json:"records"`
}

// The outer document is canonical JSON, but an immutable event payload is a
// lossless JSON string, not a raw value. Re-encoding jsontext.Value normalizes
// whitespace/escapes. Keeping its original bytes as a string avoids that loss
// without transporting an opaque owner backup or recomputing an event hash.
type publicationEventWire struct {
	db.EventExport
	Payload string `json:"payload"`
}

func publicationInvalid(err error) error {
	return fmt.Errorf("%w: %w", db.ErrFederationPublicationInvalid, err)
}

// The byte budget counts canonical per-record wire bytes, including the
// lossless payload string's escaping and record wrapper. It is charged before
// retaining each native record, bounding capture as well as the final output.
type publicationBudget struct {
	bytes, limit int64
	records      int
}

func newPublicationBudget(p db.FederationPublication, capacity int64) (*publicationBudget, error) {
	limit := int64(db.MaxRelayEnvelopeBytes)
	if capacity > 0 && capacity < limit {
		limit = capacity
	}
	w := publicationWire{Version: 1, OperationUID: p.OperationUID, SourceInstanceUID: p.SourceInstanceUID, ProjectUID: p.ProjectUID, TargetInstanceUID: p.TargetInstanceUID, ProjectName: p.ProjectName, Digest: strings.Repeat("0", 64), Records: []recordEnvelope{}}
	raw, err := publicationCanonical(w)
	if err != nil {
		return nil, err
	}
	b := &publicationBudget{bytes: int64(len(raw)), limit: limit}
	if b.bytes > b.limit {
		return nil, db.ErrFederationPublicationTooLarge
	}
	return b, nil
}

func publicationRecord(r db.ImportRecord) (recordEnvelope, error) {
	var value any = r
	if event, ok := r.(*db.EventExport); ok {
		value = publicationEventWire{EventExport: *event, Payload: string(event.Payload)}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return recordEnvelope{}, publicationInvalid(err)
	}
	return recordEnvelope{Kind: r.ImportKind(), Data: raw}, nil
}

func (b *publicationBudget) add(r db.ImportRecord) (recordEnvelope, error) {
	env, err := publicationRecord(r)
	if err != nil {
		return env, err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return env, publicationInvalid(err)
	}
	raw, err = db.CanonicalEventJSON(raw)
	if err != nil {
		return env, publicationInvalid(err)
	}
	n := int64(len(raw))
	if b.records > 0 {
		n++
	}
	if n > b.limit-b.bytes {
		return env, db.ErrFederationPublicationTooLarge
	}
	b.bytes += n
	b.records++
	return env, nil
}

// Encode freezes version 1 and hashes its canonical content
// with a SHA-256 digest (omitting the digest field itself). It never mutates p.
func Encode(p db.FederationPublication) ([]byte, error) {
	if _, err := db.ValidateFederationPublication(p); err != nil {
		return nil, err
	}
	return encodeValidated(p)
}

// encodeValidated retains the exact wire/digest checks after shared semantic
// validation. Decode reuses its validation counts without replaying the fold.
func encodeValidated(p db.FederationPublication) ([]byte, error) {
	w := publicationWire{Version: 1, OperationUID: p.OperationUID, SourceInstanceUID: p.SourceInstanceUID, ProjectUID: p.ProjectUID, TargetInstanceUID: p.TargetInstanceUID, ProjectName: p.ProjectName, Records: make([]recordEnvelope, 0, len(p.Records))}
	budget, err := newPublicationBudget(p, db.MaxRelayEnvelopeBytes)
	if err != nil {
		return nil, err
	}
	for _, r := range p.Records {
		env, err := budget.add(r)
		if err != nil {
			return nil, err
		}
		w.Records = append(w.Records, env)
	}
	digest, err := publicationDigest(w)
	if err != nil {
		return nil, err
	}
	if p.Digest != "" && p.Digest != digest {
		return nil, publicationInvalid(fmt.Errorf("digest mismatch"))
	}
	w.Digest = digest
	raw, err := publicationCanonical(w)
	if err != nil {
		return nil, err
	}
	if len(raw) > db.MaxRelayEnvelopeBytes {
		return nil, db.ErrFederationPublicationTooLarge
	}
	return raw, nil
}

func publicationCanonical(w publicationWire) ([]byte, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, publicationInvalid(err)
	}
	canonical, err := db.CanonicalEventJSON(raw)
	if err != nil {
		return nil, publicationInvalid(err)
	}
	return canonical, nil
}
func publicationDigest(w publicationWire) (string, error) {
	w.Digest = ""
	raw, err := publicationCanonical(w)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Decode accepts only the closed version-1 typed union.
// JSON v2 rejects duplicate object names, including inside raw record values.
func Decode(raw []byte) (db.FederationPublication, error) {
	p, _, err := DecodeWithCounts(raw)
	return p, err
}

// DecodeWithCounts returns the counts from the mandatory semantic validation.
// Callers can use them for previews and response checks without replaying the
// same document. Native stores still validate their independent write boundary.
func DecodeWithCounts(raw []byte) (db.FederationPublication, db.FederationPublicationCounts, error) {
	var p db.FederationPublication
	if len(raw) > db.MaxRelayEnvelopeBytes {
		return p, db.FederationPublicationCounts{}, db.ErrFederationPublicationTooLarge
	}
	var w publicationWire
	if err := json.Unmarshal(raw, &w, json.RejectUnknownMembers(true)); err != nil {
		return p, db.FederationPublicationCounts{}, publicationInvalid(err)
	}
	if w.Version != 1 || !db.ValidReplayContentHash(w.Digest) || w.Records == nil {
		return p, db.FederationPublicationCounts{}, publicationInvalid(fmt.Errorf("unsupported or incomplete publication envelope"))
	}
	digest, err := publicationDigest(w)
	if err != nil {
		return p, db.FederationPublicationCounts{}, err
	}
	if w.Digest != digest {
		return p, db.FederationPublicationCounts{}, publicationInvalid(fmt.Errorf("digest mismatch"))
	}
	p = db.FederationPublication{OperationUID: w.OperationUID, SourceInstanceUID: w.SourceInstanceUID, ProjectUID: w.ProjectUID, TargetInstanceUID: w.TargetInstanceUID, ProjectName: w.ProjectName, Digest: w.Digest, Records: make([]db.ImportRecord, 0, len(w.Records))}
	for _, env := range w.Records {
		var r db.ImportRecord
		switch env.Kind {
		case "project":
			r = &db.ProjectExport{}
		case "issue":
			r = &db.IssueExport{}
		case "comment":
			r = &db.CommentExport{}
		case "issue_label":
			r = &db.IssueLabelExport{}
		case "link":
			r = &db.LinkExport{}
		case "event":
			var event publicationEventWire
			if err := json.Unmarshal(env.Data, &event, json.RejectUnknownMembers(true)); err != nil {
				return db.FederationPublication{}, db.FederationPublicationCounts{}, publicationInvalid(err)
			}
			event.EventExport.Payload = jsontext.Value(event.Payload)
			event.ProjectUID = p.ProjectUID
			r = &event.EventExport
		default:
			return db.FederationPublication{}, db.FederationPublicationCounts{}, publicationInvalid(fmt.Errorf("unsupported record kind %q", env.Kind))
		}
		if env.Kind != "event" {
			if err := json.Unmarshal(env.Data, r, json.RejectUnknownMembers(true)); err != nil {
				return db.FederationPublication{}, db.FederationPublicationCounts{}, publicationInvalid(err)
			}
		}
		p.Records = append(p.Records, r)
	}
	// Reject alternate typed shapes (for example, omitted non-optional zero
	// fields) whose supplied digest would change when this document is encoded.
	counts, err := db.ValidateFederationPublication(p)
	if err != nil {
		return db.FederationPublication{}, db.FederationPublicationCounts{}, err
	}
	if _, err := encodeValidated(p); err != nil {
		return db.FederationPublication{}, db.FederationPublicationCounts{}, err
	}
	return p, counts, nil
}

// Capture reads one consistent native snapshot. Unsupported
// source authority is checked before records are filtered. The snapshot is
// always released before the frozen document can be sent to another daemon.
func Capture(ctx context.Context, store db.Storage, projectID int64, p db.FederationPublication) (db.FederationPublication, error) {
	if p.SourceInstanceUID != store.InstanceUID() {
		return db.FederationPublication{}, publicationInvalid(fmt.Errorf("source instance mismatch"))
	}
	snapshotter, ok := store.(snapshotter)
	if !ok {
		return db.FederationPublication{}, publicationInvalid(fmt.Errorf("source has no export snapshot"))
	}
	snapshot, release, err := snapshotter.BeginExportSnapshot(ctx)
	if err != nil {
		return db.FederationPublication{}, err
	}
	defer func() { _ = release() }()
	reader, ok := snapshot.(db.FederationPublicationSnapshotReader)
	if !ok {
		return db.FederationPublication{}, publicationInvalid(fmt.Errorf("source has no original-event snapshot reader"))
	}
	budget, err := newPublicationBudget(p, reader.FederationPublicationCapacity())
	if err != nil {
		return db.FederationPublication{}, err
	}
	f := db.ExportFilter{ProjectID: &projectID, IncludeDeleted: true}
	p.Records = nil
	p.Digest = ""
	for r, err := range snapshot.ExportProjects(ctx, f) {
		if err != nil {
			return db.FederationPublication{}, err
		}
		record := &db.ProjectExport{ID: r.ID, UID: r.UID, Name: r.Name, CreatedAt: r.CreatedAt, DeletedAt: r.DeletedAt, Metadata: r.Metadata, Revision: r.Revision}
		if _, err := budget.add(record); err != nil {
			return db.FederationPublication{}, err
		}
		p.Records = append(p.Records, record)
	}
	if len(p.Records) != 1 {
		return db.FederationPublication{}, publicationInvalid(fmt.Errorf("source project not found"))
	}
	for r, err := range snapshot.ExportMeta(ctx) {
		if err != nil {
			return db.FederationPublication{}, err
		}
		if (strings.HasPrefix(r.Key, db.RelayResetMetadataPrefix) || strings.HasPrefix(r.Key, db.RelayResetSupersededMetadataPrefix)) && strings.Contains(r.Key, p.ProjectUID) {
			return db.FederationPublication{}, publicationInvalid(fmt.Errorf("source retains a reset boundary"))
		}
	}
	// Host aliases/paths, metadata, policy, provenance, caches and sequences
	// never enter the document. Unsupported content/authority is refused rather
	// than discarded. These iterators all use the same export snapshot.
	for record, err := range snapshot.ExportAttribution(ctx, f) {
		if err != nil {
			return db.FederationPublication{}, err
		}
		return db.FederationPublication{}, publicationInvalid(fmt.Errorf("source contains unsupported %s records", record.ImportKind()))
	}
	checks := []error{
		publicationAbsent(snapshot.ExportIssueSyncBindings(ctx, f)), publicationAbsent(snapshot.ExportIssueSyncStatus(ctx, f)),
		publicationAbsent(snapshot.ExportRecurrences(ctx, f)), publicationAbsent(snapshot.ExportImportMappings(ctx, f)),
		publicationAbsent(snapshot.ExportExternalRootBindings(ctx, f)), publicationAbsent(snapshot.ExportExternalFieldStates(ctx, f)),
		publicationAbsent(snapshot.ExportFederationBindings(ctx, f)), publicationAbsent(snapshot.ExportFederationSyncStatus(ctx, f)),
		publicationAbsent(snapshot.ExportFederationQuarantine(ctx, f)), publicationAbsent(snapshot.ExportFederationEnrollments(ctx, f)),
		publicationAbsent(snapshot.ExportIssueClaims(ctx, f)), publicationAbsent(snapshot.ExportPendingClaimRequests(ctx, f)),
		publicationAbsent(snapshot.ExportCronJobs(ctx, f)), publicationAbsent(snapshot.ExportCronWorkflows(ctx, f)), publicationAbsent(snapshot.ExportCronRuns(ctx, f)),
		publicationAbsent(snapshot.ExportPurgeLog(ctx, f)), publicationAbsent(snapshot.ExportProjectPurgeLog(ctx, f)),
	}
	for _, err := range checks {
		if err != nil {
			return db.FederationPublication{}, err
		}
	}
	if err := publicationAppend(&p, budget, snapshot.ExportIssues(ctx, f)); err != nil {
		return db.FederationPublication{}, err
	}
	if err := publicationAppend(&p, budget, snapshot.ExportComments(ctx, f)); err != nil {
		return db.FederationPublication{}, err
	}
	if err := publicationAppend(&p, budget, snapshot.ExportIssueLabels(ctx, f)); err != nil {
		return db.FederationPublication{}, err
	}
	if err := publicationAppend(&p, budget, snapshot.ExportLinks(ctx, f)); err != nil {
		return db.FederationPublication{}, err
	}
	if err := publicationAppend(&p, budget, reader.ExportFederationPublicationEvents(ctx, projectID)); err != nil {
		return db.FederationPublication{}, err
	}
	// Encoding validates original content before canonicalization; decoding
	// returns a deep defensive copy, including pointers and raw JSON fields.
	raw, err := Encode(p)
	if err != nil {
		return db.FederationPublication{}, err
	}
	size, err := reader.FederationPublicationCheckpointSize(ctx, projectID)
	if err != nil {
		return db.FederationPublication{}, err
	}
	if err := size.CapacityError(p.ProjectUID); err != nil {
		return db.FederationPublication{}, fmt.Errorf("%w: %w", db.ErrFederationPublicationTooLarge, err)
	}
	return Decode(raw)
}

func publicationAbsent[T any](records iter.Seq2[T, error]) error {
	for r, err := range records {
		if err != nil {
			return err
		}
		record := any(&r).(db.ImportRecord)
		return publicationInvalid(fmt.Errorf("source contains unsupported %s records", record.ImportKind()))
	}
	return nil
}
func publicationAppend[T any](p *db.FederationPublication, budget *publicationBudget, records iter.Seq2[T, error]) error {
	for r, err := range records {
		if err != nil {
			return err
		}
		record, ok := any(&r).(db.ImportRecord)
		if !ok {
			return publicationInvalid(fmt.Errorf("unsupported source record"))
		}
		if _, err := budget.add(record); err != nil {
			return err
		}
		p.Records = append(p.Records, record)
	}
	return nil
}
