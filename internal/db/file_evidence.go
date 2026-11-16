package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// Validate only new file metadata; legacy close evidence remains opaque. This
// shared path is used by JSONL restore, federation push/pull and relay ingest.
func validateFileEvidencePayload(payload jsontext.Value) error {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	} // Legacy/header-only replay.
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(payload, &fields); err != nil {
		return fmt.Errorf("%w: invalid close payload", ErrFederationIngestValidation)
	}
	var entries []jsontext.Value
	if json.Unmarshal(fields["evidence"], &entries) != nil {
		return nil
	} // Legacy evidence stays opaque.
	for _, raw := range entries {
		var entry map[string]jsontext.Value
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(entry["type"], &kind)
		file, hasFile := entry["file"]
		if kind != "file" && (!hasFile || bytes.Equal(bytes.TrimSpace(file), []byte("null"))) {
			continue
		}
		var e Evidence
		if err := json.Unmarshal(raw, &e, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("%w: invalid file evidence", ErrFederationIngestValidation)
		}
		if err := ValidateFileEvidence([]Evidence{e}); err != nil {
			return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
		}
	}
	return nil
}

// ValidateFileEvidence checks retained metadata before local writes and replay.
// It does not reinterpret legacy evidence or contact the source.
func ValidateFileEvidence(evidence []Evidence) error {
	for _, e := range evidence {
		if err := ValidateFileEvidenceFields(e); err != nil {
			return err
		}
		if e.Type == "file" {
			if err := e.File.ValidateRetained(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateFileEvidenceFields enforces the file union invariant on all writes,
// including owner-local TUI closes and offline replay. It never reads a source.
func ValidateFileEvidenceFields(e Evidence) error {
	if e.Type != "file" {
		if e.File != nil {
			return fmt.Errorf("file payload requires file type")
		}
		return nil
	}
	if e.File == nil {
		return fmt.Errorf("file reference missing")
	}
	if e.Comment != "" || e.SHA != "" || e.URL != "" || e.Command != "" || len(e.Paths) > 0 || e.Account != "" || e.Rationale != "" || e.IssueRef != "" || e.AttachmentID != "" || e.Attachment != nil {
		return fmt.Errorf("file evidence accepts only file reference")
	}
	return nil
}
