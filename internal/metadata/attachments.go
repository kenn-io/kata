package metadata

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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

// rejectNull refuses a null size, which would otherwise decode as zero bytes.
var rejectNull = json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, _ *int64) error {
	if dec.PeekKind() == 'n' {
		return errors.New("size is null")
	}
	return errors.ErrUnsupported
}))

// Valid decodes supported references at use time. Storage remains opaque.
// A missing size keeps the -1 sentinel and fails the size check.
func Valid(raw jsontext.Value) (Attachment, bool) {
	e := Attachment{Size: -1}
	if json.Unmarshal(raw, &e, json.RejectUnknownMembers(true), rejectNull) != nil {
		return e, false
	}
	return e, ValidAttachmentName(e.Name) && e.MediaType != "" && e.Size >= 0 && e.DocbankVersion != uuid.Nil()
}

// ValidAttachmentName checks the basename shared by uploads and stored references.
func ValidAttachmentName(name string) bool {
	return utf8.ValidString(name) && name != "" && len(name) <= 255 && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.ContainsFunc(name, unicode.IsControl)
}
