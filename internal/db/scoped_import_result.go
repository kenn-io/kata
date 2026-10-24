package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

// ScopedImportOutcome is the public V3 terminal arm. Root execution must supply
// a randomized commitment to closed private metadata; it discloses no native
// output identities, cardinality, clocks, mappings or root originals.
// Decoding or signing this value does not execute or accept a native import.
type ScopedImportOutcome struct {
	Commitment string `json:"commitment"`
}

func (r *SourceResult) UnmarshalJSON(raw []byte) error {
	type plain SourceResult
	var value plain
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if outcome, present := fields["import_outcome"]; present && bytes.Equal(bytes.TrimSpace(outcome), []byte("null")) {
		return errors.New("import outcome cannot be null")
	}
	*r = SourceResult(value)
	return nil
}
