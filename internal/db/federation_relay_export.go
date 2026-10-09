package db

import (
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// RelayOutboxExport and the following records are daemon-local delivery state.
// Only complete owner backups carry them; project transfer never grants their
// retained transport authority or treats them as source events.
type RelayOutboxExport struct {
	ID           int64  `json:"id"`
	ProjectUID   string `json:"project_uid"`
	BindingUID   string `json:"binding_uid"`
	Stream       string `json:"stream"`
	ResetEpoch   int64  `json:"reset_epoch"`
	SourceUID    string `json:"source_uid"`
	SourceHash   string `json:"source_hash"`
	Envelope     string `json:"envelope"`
	Emitted      bool   `json:"emitted"`
	Acknowledged bool   `json:"acknowledged"`
}

// ImportKind identifies this record for JSONL import and export.
func (*RelayOutboxExport) ImportKind() string { return "federation_relay_outbox" }

// RelayInboxExport retains accepted hop identities for complete owner backup and restore.
type RelayInboxExport struct {
	ProjectUID     string `json:"project_uid"`
	BindingUID     string `json:"binding_uid"`
	Stream         string `json:"stream"`
	ResetEpoch     int64  `json:"reset_epoch"`
	Sequence       int64  `json:"sequence"`
	SourceUID      string `json:"source_uid"`
	SourceHash     string `json:"source_hash"`
	EnvelopeDigest string `json:"envelope_digest"`
	Envelope       string `json:"envelope"`
	Accepted       bool   `json:"accepted"`
}

// ImportKind identifies this record for JSONL import and export.
func (*RelayInboxExport) ImportKind() string { return "federation_relay_inbox" }

// RelayCursorExport retains independent emitted and accepted stream prefixes for owner backups.
type RelayCursorExport struct {
	ProjectUID          string `json:"project_uid"`
	BindingUID          string `json:"binding_uid"`
	Stream              string `json:"stream"`
	ResetEpoch          int64  `json:"reset_epoch"`
	OfferedThrough      int64  `json:"offered_through"`
	AcceptedThrough     int64  `json:"accepted_through"`
	EmittedThrough      int64  `json:"emitted_through"`
	AcknowledgedThrough int64  `json:"acknowledged_through"`
}

// ImportKind identifies this record for JSONL import and export.
func (*RelayCursorExport) ImportKind() string { return "federation_relay_cursors" }

// ValidateRelayDeliveryRecord checks backup delivery fields against their retained hop envelope.
func ValidateRelayDeliveryRecord(record ImportRecord) error {
	var project, binding, stream, source, hash, digest, raw string
	var epoch, sequence int64
	switch r := record.(type) {
	case *RelayOutboxExport:
		if r == nil || (r.Acknowledged && !r.Emitted) {
			return errors.New("invalid relay outbox record")
		}
		project, binding, stream, epoch, sequence, source, hash, raw = r.ProjectUID, r.BindingUID, r.Stream, r.ResetEpoch, r.ID, r.SourceUID, r.SourceHash, r.Envelope
	case *RelayInboxExport:
		if r == nil {
			return errors.New("nil relay inbox record")
		}
		project, binding, stream, epoch, sequence, source, hash, digest, raw = r.ProjectUID, r.BindingUID, r.Stream, r.ResetEpoch, r.Sequence, r.SourceUID, r.SourceHash, r.EnvelopeDigest, r.Envelope
	case *RelayCursorExport:
		if r == nil || !uid.Valid(r.ProjectUID) || !uid.Valid(r.BindingUID) || !supportedRelayStream(r.Stream) || r.ResetEpoch <= 0 || r.OfferedThrough < 0 || r.AcceptedThrough < 0 || r.EmittedThrough < 0 || r.AcknowledgedThrough < 0 || r.AcceptedThrough > r.OfferedThrough || r.AcknowledgedThrough > r.EmittedThrough {
			return errors.New("invalid relay cursor record")
		}
		return nil
	default:
		return errors.New("unsupported relay delivery record")
	}
	var envelope RelayEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return err
	}
	if envelope.ProjectUID != project || envelope.BindingUID != binding || envelope.Stream != stream || envelope.Epoch != epoch || envelope.Sequence != sequence || envelope.SourceUID != source || envelope.SourceHash != hash || (digest != "" && envelope.Digest != digest) {
		return errors.New("relay record differs from retained envelope")
	}
	authority := RelayHopAuthority{BindingUID: binding, ProjectUID: project, AuthorityUID: envelope.AuthorityUID, SenderInstanceUID: envelope.SenderInstanceUID, ReceiverInstanceUID: envelope.ReceiverInstanceUID, Epoch: epoch}
	return ValidateRelayEnvelope(authority, envelope)
}
