package db

import (
	"encoding/json/v2"
	"fmt"
)

// EncodeRelaySourceEvent retains payload bytes separately from JSON metadata.
// jsontext.Value reserialization can normalize whitespace; raw bytes preserve
// the source representation across every forwarding hop.
func EncodeRelaySourceEvent(event RemoteEvent) ([]byte, error) {
	if _, _, err := ValidateRemoteEventContentHash(event); err != nil {
		return nil, err
	}
	payload := []byte(event.Payload)
	event.Payload = nil
	return json.Marshal(struct {
		Event   RemoteEvent `json:"event"`
		Payload []byte      `json:"payload"`
	}{event, payload})
}

// DecodeRelaySourceEvent verifies the exact retained source body and content hash.
func DecodeRelaySourceEvent(body []byte) (RemoteEvent, error) {
	if len(body) > MaxRelayEnvelopeBytes {
		return RemoteEvent{}, fmt.Errorf("%w: relay source exceeds byte limit", ErrFederationIngestValidation)
	}
	var wire struct {
		Event   RemoteEvent `json:"event"`
		Payload []byte      `json:"payload"`
	}
	if err := json.Unmarshal(body, &wire, json.RejectUnknownMembers(true)); err != nil {
		return RemoteEvent{}, fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
	}
	if len(wire.Event.Payload) != 0 {
		return RemoteEvent{}, fmt.Errorf("%w: relay source contains alternate payload", ErrFederationIngestValidation)
	}
	wire.Event.Payload = wire.Payload
	if _, _, err := ValidateRemoteEventContentHash(wire.Event); err != nil {
		return RemoteEvent{}, fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
	}
	return wire.Event, nil
}
