package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/metadata"
)

// ErrAttachmentEvidence identifies a missing or invalid close-time reference.
var ErrAttachmentEvidence = errors.New("invalid attachment evidence")

// ResolveAttachmentEvidence validates references against the transaction's
// current issue and independently freezes their descriptors. It never reads
// Docbank. Caller-supplied descriptors are replaced with server-validated data.
func ResolveAttachmentEvidence(issue Issue, evidence []Evidence) ([]Evidence, error) {
	out := slices.Clone(evidence)
	var values map[string]jsontext.Value
	for i, e := range evidence {
		out[i].Attachment = nil
		if e.Type != "attachment" {
			continue
		}
		missing := func() error {
			return fmt.Errorf("evidence[%d] attachment %s not found on issue: %w", i, e.AttachmentID, ErrAttachmentEvidence)
		}
		if values == nil {
			if json.Unmarshal([]byte(issue.Metadata), &values) != nil {
				return nil, missing()
			}
		}
		entry, ok := metadata.Valid(values[metadata.AttachmentPrefix+e.AttachmentID])
		if e.AttachmentID == "" || !ok {
			return nil, missing()
		}
		out[i].Attachment = &entry
	}
	return out, nil
}
