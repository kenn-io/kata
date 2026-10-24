package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/uid"
)

type ScopedDeliveryDirection string

const (
	ScopedDeliveryInbound  ScopedDeliveryDirection = "inbound"
	ScopedDeliveryOutbound ScopedDeliveryDirection = "outbound"
)

type ScopedDeliveryChannel string

const (
	ScopedDeliveryView       ScopedDeliveryChannel = "view"
	ScopedDeliverySource     ScopedDeliveryChannel = "source"
	ScopedDeliveryResult     ScopedDeliveryChannel = "result"
	ScopedDeliveryCoverage   ScopedDeliveryChannel = "coverage"
	ScopedDeliveryControl    ScopedDeliveryChannel = "control"
	ScopedDeliveryConversion ScopedDeliveryChannel = "conversion"
)

// Direction is local custody direction, not part of the remote hop header.
// Sequence numbers are contiguous positive values within the complete stream.
// Allocate an outbound sequence in the durable insertion transaction, never
// from a global SQL sequence. Missing inbound numbers cannot advance a prefix.
type ScopedDeliveryStream struct {
	BindingUID string                  `json:"binding_uid"`
	Epoch      int64                   `json:"epoch"`
	Direction  ScopedDeliveryDirection `json:"direction"`
	Channel    ScopedDeliveryChannel   `json:"channel"`
}
type ScopedDeliveryRef struct {
	ScopedDeliveryStream
	Sequence int64 `json:"sequence"`
}
type ScopedDeliveryRecord struct {
	ScopedDeliveryRef
	ObjectUID     string             `json:"object_uid"`
	ObjectDigest  string             `json:"object_digest"`
	OfferedDigest string             `json:"offered_digest"`
	HeaderBytes   []byte             `json:"header_bytes"`
	EnvelopeBytes []byte             `json:"envelope_bytes,format:emitnull"`
	Accepted      bool               `json:"accepted"`
	Emitted       bool               `json:"emitted"`
	Acknowledged  bool               `json:"acknowledged"`
	Upstream      *ScopedDeliveryRef `json:"upstream"`
}
type ScopedDeliveryCursor struct {
	ScopedDeliveryStream
	ReceivedSequence     int64  `json:"received_sequence"`
	ReceivedDigest       string `json:"received_digest"`
	AcceptedSequence     int64  `json:"accepted_sequence"`
	AcceptedDigest       string `json:"accepted_digest"`
	EmittedSequence      int64  `json:"emitted_sequence"`
	EmittedDigest        string `json:"emitted_digest"`
	AcknowledgedSequence int64  `json:"acknowledged_sequence"`
	AcknowledgedDigest   string `json:"acknowledged_digest"`
}

const ScopedDeliveryDomain = "kata.scoped.delivery.v1"

// HeaderBytes is the canonical immutable hop transcript, with body commitment.
// Existing authenticated HTTP request signing protects this hop; object root
// signatures remain in the opaque body under their separate record domains.
// HeaderDigest commits only HeaderBytes. OfferedDigest commits the exact entire
// encoded envelope, including its base64 body. Neither is ObjectDigest.
type ScopedDeliveryHeader struct {
	Version             int                   `json:"version"`
	Domain              string                `json:"domain"`
	BindingUID          string                `json:"binding_uid"`
	Epoch               int64                 `json:"epoch"`
	Channel             ScopedDeliveryChannel `json:"channel"`
	Sequence            int64                 `json:"sequence"`
	AuthorityUID        string                `json:"authority_uid"`
	SenderInstanceUID   string                `json:"sender_instance_uid"`
	ReceiverInstanceUID string                `json:"receiver_instance_uid"`
	Path                []string              `json:"path"`
	ObjectUID           string                `json:"object_uid"`
	ObjectDigest        string                `json:"object_digest"`
	BodyDigest          string                `json:"body_digest"`
}
type ScopedDeliveryEnvelope struct {
	HeaderBytes  []byte `json:"header_bytes"`
	Body         []byte `json:"body"`
	HeaderDigest string `json:"header_digest"`
}

func validScopedDeliveryChannel(channel ScopedDeliveryChannel) bool {
	switch channel {
	case ScopedDeliveryView, ScopedDeliverySource, ScopedDeliveryResult, ScopedDeliveryCoverage, ScopedDeliveryControl, ScopedDeliveryConversion:
		return true
	}
	return false
}
func (s ScopedDeliveryStream) Validate() error {
	if !uid.Valid(s.BindingUID) || s.Epoch <= 0 || (s.Direction != ScopedDeliveryInbound && s.Direction != ScopedDeliveryOutbound) || !validScopedDeliveryChannel(s.Channel) {
		return errors.New("invalid scoped delivery stream")
	}
	return nil
}
func (r ScopedDeliveryRef) Validate() error {
	if err := r.ScopedDeliveryStream.Validate(); err != nil {
		return err
	}
	if r.Sequence <= 0 {
		return errors.New("invalid scoped delivery sequence")
	}
	return nil
}
func (h ScopedDeliveryHeader) Validate() error {
	if h.Version != ScopedRelayProtocolVersion || h.Domain != ScopedDeliveryDomain || !uid.Valid(h.BindingUID) || h.Epoch <= 0 || h.Sequence <= 0 || !validScopedDeliveryChannel(h.Channel) || !uid.Valid(h.AuthorityUID) || !uid.Valid(h.SenderInstanceUID) || !uid.Valid(h.ReceiverInstanceUID) || h.SenderInstanceUID == h.ReceiverInstanceUID || !uid.Valid(h.ObjectUID) || !scopedDigest(h.ObjectDigest) || !scopedDigest(h.BodyDigest) {
		return errors.New("invalid scoped delivery header")
	}
	if len(h.Path) == 0 || len(h.Path) > MaxRelayPathNodes || h.Path[len(h.Path)-1] != h.SenderInstanceUID {
		return errors.New("invalid scoped delivery path")
	}
	seen := map[string]bool{h.ReceiverInstanceUID: true}
	for _, node := range h.Path {
		if !uid.Valid(node) || seen[node] {
			return errors.New("cyclic scoped delivery path")
		}
		seen[node] = true
	}
	return nil
}

// ScopedOfferedDigest commits exact bytes. Callers retain those bytes verbatim
// until compaction is safe; they never reconstruct a retry from current state.
func ScopedOfferedDigest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

func EncodeScopedDelivery(header ScopedDeliveryHeader, body []byte) ([]byte, error) {
	if len(body) > MaxScopedCheckpointBytes {
		return nil, errors.New("scoped delivery body too large")
	}
	header.BodyDigest = ScopedOfferedDigest(body)
	if err := header.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	envelope, err := json.Marshal(ScopedDeliveryEnvelope{HeaderBytes: canonical, Body: body, HeaderDigest: ScopedOfferedDigest(canonical)})
	if err != nil {
		return nil, err
	}
	if len(envelope) > MaxScopedEnvelopeBytes {
		return nil, errors.New("scoped wire envelope too large")
	}
	return envelope, nil
}

func decodeScopedDelivery(raw []byte) (ScopedDeliveryEnvelope, ScopedDeliveryHeader, error) {
	var envelope ScopedDeliveryEnvelope
	var header ScopedDeliveryHeader
	if len(raw) == 0 || len(raw) > MaxScopedEnvelopeBytes {
		return envelope, header, errors.New("invalid scoped envelope size")
	}
	if err := DecodeScopedRecord(raw, &envelope); err != nil {
		return envelope, header, err
	}
	if len(envelope.HeaderBytes) == 0 || len(envelope.HeaderBytes) > 64<<10 || len(envelope.Body) > MaxScopedCheckpointBytes || ScopedOfferedDigest(envelope.HeaderBytes) != envelope.HeaderDigest {
		return envelope, header, errors.New("scoped header digest or size mismatch")
	}
	if err := ValidateScopedSignedBytes(envelope.HeaderBytes); err != nil {
		return envelope, header, err
	}
	if err := DecodeScopedRecord(envelope.HeaderBytes, &header); err != nil {
		return envelope, header, err
	}
	if ScopedOfferedDigest(envelope.Body) != header.BodyDigest {
		return envelope, header, errors.New("scoped body commitment mismatch")
	}
	return envelope, header, nil
}
func DecodeScopedDelivery(raw []byte) (ScopedDeliveryHeader, []byte, error) {
	envelope, header, err := decodeScopedDelivery(raw)
	if err != nil {
		return header, nil, err
	}
	return header, bytes.Clone(envelope.Body), nil
}

// ScopedDeliveryHeaderBytes extracts the EXACT verified transcript. Tombstones
// retain this accessor's bytes; never rebuild headers from a later enrollment.
func ScopedDeliveryHeaderBytes(raw []byte) ([]byte, error) {
	envelope, _, err := decodeScopedDelivery(raw)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(envelope.HeaderBytes), nil
}

func (r ScopedDeliveryRecord) Validate() error {
	if err := r.ScopedDeliveryRef.Validate(); err != nil {
		return err
	}
	if !uid.Valid(r.ObjectUID) || !scopedDigest(r.ObjectDigest) || !scopedDigest(r.OfferedDigest) || len(r.HeaderBytes) == 0 {
		return errors.New("invalid retained scoped identity")
	}
	if err := ValidateScopedSignedBytes(r.HeaderBytes); err != nil {
		return err
	}
	var h ScopedDeliveryHeader
	if err := DecodeScopedRecord(r.HeaderBytes, &h); err != nil {
		return err
	}
	if h.BindingUID != r.BindingUID || h.Epoch != r.Epoch || h.Channel != r.Channel || h.Sequence != r.Sequence || h.ObjectUID != r.ObjectUID || h.ObjectDigest != r.ObjectDigest {
		return errors.New("retained header disagrees with row")
	}
	if r.Direction == ScopedDeliveryInbound && (r.Emitted || r.Acknowledged) || r.Direction == ScopedDeliveryOutbound && (r.Accepted || (r.Acknowledged && !r.Emitted)) {
		return errors.New("invalid direction/state union")
	}
	if r.Upstream != nil {
		if err := r.Upstream.Validate(); err != nil {
			return err
		}
		if r.Direction != ScopedDeliveryOutbound || r.Upstream.Direction != ScopedDeliveryInbound {
			return errors.New("invalid upstream custody direction")
		}
	}
	if r.EnvelopeBytes == nil {
		if !(r.Direction == ScopedDeliveryInbound && r.Accepted || r.Direction == ScopedDeliveryOutbound && r.Acknowledged) {
			return errors.New("unsettled envelope compacted")
		}
		// The body commitment remains provable, but independent custody and epoch
		// retirement proofs are transaction-level duties, not inferred here.
		return nil
	}
	header, err := ScopedDeliveryHeaderBytes(r.EnvelopeBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(header, r.HeaderBytes) || ScopedOfferedDigest(r.EnvelopeBytes) != r.OfferedDigest {
		return errors.New("retained exact offer mismatch")
	}
	return nil
}

// SameScopedDeliveryIdentity compares old per-sequence identity even after
// compaction. It never treats a cursor's endpoint as proof for another row.
func SameScopedDeliveryIdentity(a, b ScopedDeliveryRecord) bool {
	return a.ScopedDeliveryRef == b.ScopedDeliveryRef && a.ObjectUID == b.ObjectUID && a.ObjectDigest == b.ObjectDigest && a.OfferedDigest == b.OfferedDigest && bytes.Equal(a.HeaderBytes, b.HeaderBytes)
}
func (c ScopedDeliveryCursor) Validate() error {
	if err := c.ScopedDeliveryStream.Validate(); err != nil {
		return err
	}
	for _, pair := range []struct {
		n int64
		d string
	}{{c.ReceivedSequence, c.ReceivedDigest}, {c.AcceptedSequence, c.AcceptedDigest}, {c.EmittedSequence, c.EmittedDigest}, {c.AcknowledgedSequence, c.AcknowledgedDigest}} {
		if pair.n < 0 || (pair.n == 0 && pair.d != "") || (pair.n > 0 && !scopedDigest(pair.d)) {
			return errors.New("invalid scoped cursor endpoint")
		}
	}
	if c.Direction == ScopedDeliveryInbound {
		if c.EmittedSequence != 0 || c.AcknowledgedSequence != 0 || c.AcceptedSequence > c.ReceivedSequence {
			return errors.New("invalid inbound cursor union")
		}
	} else if c.ReceivedSequence != 0 || c.AcceptedSequence != 0 || c.AcknowledgedSequence > c.EmittedSequence {
		return errors.New("invalid outbound cursor union")
	}
	return nil
}

// ValidateScopedDeliveryPrefix verifies a proposed advance against retained
// per-sequence rows. The caller validates row commitments and cursor endpoints
// in the same fenced transaction, and updates flags/cursors atomically.
func ValidateScopedDeliveryPrefix(cursor ScopedDeliveryCursor, through int64, phase string, rows []ScopedDeliveryRecord) error {
	if err := cursor.Validate(); err != nil {
		return err
	}
	var start int64
	switch phase {
	case "received":
		if cursor.Direction != ScopedDeliveryInbound {
			return errors.New("received requires inbound")
		}
		start = cursor.ReceivedSequence
	case "accepted":
		if cursor.Direction != ScopedDeliveryInbound {
			return errors.New("accepted requires inbound")
		}
		start = cursor.AcceptedSequence
	case "emitted":
		if cursor.Direction != ScopedDeliveryOutbound {
			return errors.New("emitted requires outbound")
		}
		start = cursor.EmittedSequence
	case "acknowledged":
		if cursor.Direction != ScopedDeliveryOutbound {
			return errors.New("acknowledged requires outbound")
		}
		start = cursor.AcknowledgedSequence
	default:
		return errors.New("unknown scoped delivery prefix phase")
	}
	if through < start {
		return errors.New("scoped prefix moved backwards")
	}
	// Scan only provided evidence, never iterate an attacker-supplied int64 gap.
	indexed := map[int64]ScopedDeliveryRecord{}
	for _, row := range rows {
		if row.ScopedDeliveryStream != cursor.ScopedDeliveryStream || row.Sequence <= start || row.Sequence > through {
			return errors.New("prefix evidence outside stream or interval")
		}
		if _, ok := indexed[row.Sequence]; ok {
			return errors.New("duplicate prefix evidence")
		}
		indexed[row.Sequence] = row
	}
	if through-start != int64(len(indexed)) {
		return errors.New("missing contiguous scoped delivery evidence")
	}
	for _, row := range indexed {
		if phase == "accepted" && !row.Accepted || phase == "emitted" && !row.Emitted || phase == "acknowledged" && (!row.Emitted || !row.Acknowledged) {
			return errors.New("unresolved scoped delivery prefix member")
		}
	}
	return nil
}
