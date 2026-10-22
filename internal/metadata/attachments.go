package metadata

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"unicode"
	"unicode/utf8"
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

// Valid decodes supported references at use time. Storage remains opaque.
func Valid(raw jsontext.Value) (Attachment, bool) {
	var e Attachment
	if json.Unmarshal(raw, &e, json.RejectUnknownMembers(true)) != nil {
		return e, false
	}
	return e, ValidAttachmentName(e.Name) && e.MediaType != "" && e.Size >= 0 && e.DocbankVersion != uuid.Nil()
}

// ValidAttachmentName checks the basename shared by uploads and stored references.
func ValidAttachmentName(name string) bool {
	return utf8.ValidString(name) && name != "" && len(name) <= 255 && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.ContainsFunc(name, unicode.IsControl)
}
