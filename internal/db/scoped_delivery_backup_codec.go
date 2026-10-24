package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// This adapter preserves the frozen record field/wire shape. A named nullable
// byte codec handles the one SQL-nullable byte field; no global JSON option or
// authenticated header/body re-encoding participates in owner backup.
type scopedDeliveryBackupWire struct {
	ScopedDeliveryRef
	ObjectUID     string              `json:"object_uid"`
	ObjectDigest  string              `json:"object_digest"`
	OfferedDigest string              `json:"offered_digest"`
	HeaderBytes   []byte              `json:"header_bytes"`
	EnvelopeBytes ScopedNullableBytes `json:"envelope_bytes"`
	Accepted      bool                `json:"accepted"`
	Emitted       bool                `json:"emitted"`
	Acknowledged  bool                `json:"acknowledged"`
	Upstream      *ScopedDeliveryRef  `json:"upstream"`
}

func (r ScopedDeliveryRecord) MarshalJSON() ([]byte, error) {
	return json.Marshal(scopedDeliveryBackupWire{r.ScopedDeliveryRef, r.ObjectUID, r.ObjectDigest, r.OfferedDigest, r.HeaderBytes, ScopedNullableBytes(r.EnvelopeBytes), r.Accepted, r.Emitted, r.Acknowledged, r.Upstream})
}
func (r *ScopedDeliveryRecord) UnmarshalJSON(raw []byte) error {
	if r == nil {
		return errors.New("nil scoped delivery backup target")
	}
	// Check presence/nullability before bool/int/string zero coercion. Parsing
	// this closed outer metadata object never modifies its base64 retained bytes.
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for _, name := range []string{"binding_uid", "epoch", "direction", "channel", "sequence", "object_uid", "object_digest", "offered_digest", "header_bytes", "envelope_bytes", "accepted", "emitted", "acknowledged", "upstream"} {
		value, ok := members[name]
		if !ok {
			return fmt.Errorf("scoped delivery backup missing %s", name)
		}
		if name != "envelope_bytes" && name != "upstream" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("scoped delivery backup forbids null %s", name)
		}
	}
	var wire scopedDeliveryBackupWire
	if err := json.Unmarshal(raw, &wire, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*r = ScopedDeliveryRecord{ScopedDeliveryRef: wire.ScopedDeliveryRef, ObjectUID: wire.ObjectUID, ObjectDigest: wire.ObjectDigest, OfferedDigest: wire.OfferedDigest, HeaderBytes: wire.HeaderBytes, EnvelopeBytes: []byte(wire.EnvelopeBytes), Accepted: wire.Accepted, Emitted: wire.Emitted, Acknowledged: wire.Acknowledged, Upstream: wire.Upstream}
	return nil
}
