package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// EventHashInput is the portable event content used for replay de-duplication.
// It deliberately excludes local row IDs and projection-only display fields.
type EventHashInput struct {
	UID               string          `json:"uid"`
	OriginInstanceUID string          `json:"origin_instance_uid"`
	ProjectUID        string          `json:"project_uid"`
	ProjectName       string          `json:"-"`
	IssueUID          *string         `json:"issue_uid,omitempty"`
	RelatedIssueUID   *string         `json:"related_issue_uid,omitempty"`
	Type              string          `json:"type"`
	Actor             string          `json:"actor"`
	HLCPhysicalMS     int64           `json:"hlc_physical_ms"`
	HLCCounter        int64           `json:"hlc_counter"`
	CreatedAt         string          `json:"created_at"`
	Payload           json.RawMessage `json:"payload"`
}

// EventContentHash returns the lowercase hex SHA-256 of the canonical hash input.
func EventContentHash(in EventHashInput) (string, error) {
	payload, err := CanonicalEventJSON(in.Payload)
	if err != nil {
		return "", err
	}
	in.Payload = payload
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("marshal hash input: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalEventJSON normalizes a JSON payload into encoding/json's stable object
// key order and compact representation.
func CanonicalEventJSON(raw json.RawMessage) (json.RawMessage, error) {
	return canonicalJSONPreserveNumbers(raw)
}

func canonicalJSONPreserveNumbers(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical payload: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical payload: %w", err)
	}
	return json.RawMessage(b), nil
}
