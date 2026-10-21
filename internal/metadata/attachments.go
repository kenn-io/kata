package metadata

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"unicode"
	"uuid"
)

// AttachmentPrefix gives each file an independent, opaque metadata key.
const AttachmentPrefix = "attachment."

// Attachment refers to an immutable Docbank content version.
type Attachment struct {
	Name           string    `json:"name"`
	MediaType      string    `json:"media_type"`
	Size           int64     `json:"size"`
	DocbankVersion uuid.UUID `json:"docbank_version"`
}

// Valid decodes a reference at use time. Unknown fields stay opaque in storage.
func Valid(raw jsontext.Value) (Attachment, bool) {
	var e Attachment
	if json.Unmarshal(raw, &e) != nil {
		return e, false
	}
	return e, e.Name != "" && len(e.Name) <= 255 && e.Name != "." && e.Name != ".." &&
		!strings.ContainsAny(e.Name, `/\`) && !strings.ContainsFunc(e.Name, unicode.IsControl) &&
		e.MediaType != "" && e.Size >= 0 && e.DocbankVersion != uuid.Nil()
}
