package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/metadata"
)

// EventHashInput is the portable event content used for replay de-duplication.
// It deliberately excludes local row IDs and projection-only display fields.
type EventHashInput struct {
	UID               string         `json:"uid"`
	OriginInstanceUID string         `json:"origin_instance_uid"`
	ProjectUID        string         `json:"project_uid"`
	ProjectName       string         `json:"-"`
	IssueUID          *string        `json:"issue_uid,omitempty"`
	RelatedIssueUID   *string        `json:"related_issue_uid,omitempty"`
	Type              string         `json:"type"`
	Actor             string         `json:"actor"`
	HLCPhysicalMS     int64          `json:"hlc_physical_ms"`
	HLCCounter        int64          `json:"hlc_counter"`
	CreatedAt         string         `json:"created_at"`
	Payload           jsontext.Value `json:"payload"`
}

// EventContentHash returns the lowercase hex SHA-256 of the canonical hash input.
func EventContentHash(in EventHashInput) (string, error) {
	payload, err := CanonicalEventJSON(in.Payload)
	if err != nil {
		return "", err
	}
	in.Payload = payload
	b, err := json.Marshal(in, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	if err != nil {
		return "", fmt.Errorf("marshal hash input: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalEventJSON normalizes a JSON payload into encoding/json's stable object
// key order and compact representation.
func CanonicalEventJSON(raw jsontext.Value) (jsontext.Value, error) {
	return canonicalJSONPreserveNumbers(raw)
}

func canonicalJSONPreserveNumbers(raw jsontext.Value) (jsontext.Value, error) {
	if len(raw) == 0 {
		return jsontext.Value(`{}`), nil
	}
	var value jsontext.Value
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("canonical payload: %w", err)
	}
	return jsontext.Value(metadata.NormalizeJSON(value)), nil
}
