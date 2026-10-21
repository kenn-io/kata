package metadata

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"mime"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// DocbankReference pins immutable bytes in one Docbank vault. It contains no URL or credential.
type DocbankReference struct {
	VaultUID         string `json:"vault_uid"`
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	ContentSHA256    string `json:"content_sha256"`
}

// Attachment is locally admitted metadata. Peer entries remain opaque until use.
type Attachment struct {
	Name      string           `json:"name"`
	MediaType string           `json:"media_type"`
	Size      int64            `json:"size"`
	AddedBy   string           `json:"added_by"`
	AddedAt   string           `json:"added_at"`
	Docbank   DocbankReference `json:"docbank"`
}

// Attachments keeps unknown peer fields intact during local add/remove operations.
type Attachments map[string]jsontext.Value

var attachmentUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var attachmentHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DecodeAttachments checks the shape only; malformed peer entries remain removable.
func DecodeAttachments(raw jsontext.Value) (Attachments, error) {
	m := Attachments{}
	if len(raw) == 0 || string(raw) == "null" {
		return m, nil
	}
	if raw.Kind() != '{' {
		return nil, fmt.Errorf("%w: attachments must be an object", ErrInvalidValue)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: attachments: %v", ErrInvalidValue, err)
	}
	for id, e := range m {
		if !reULID.MatchString(id) || e.Kind() != '{' {
			return nil, fmt.Errorf("%w: attachments require ULID keys and object entries", ErrInvalidValue)
		}
	}
	return m, nil
}

// Encode retains raw entry values, including fields unknown to this daemon.
func (m Attachments) Encode() jsontext.Value {
	keys := make([]string, 0, len(m))
	for id := range m {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, id := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(id)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m[id])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// ValidateEntry checks an entry immediately before local admission or dereferencing.
func ValidateEntry(id string, e Attachment) error {
	fail := func(why string) error { return fmt.Errorf("%w: attachment %s", ErrInvalidValue, why) }
	if !reULID.MatchString(id) {
		return fail("id must be a ULID")
	}
	if e.Name == "" || len(e.Name) > 255 || strings.ContainsAny(e.Name, `/\`) || strings.ContainsFunc(e.Name, unicode.IsControl) || e.Name == "." || e.Name == ".." {
		return fail("name must be a safe basename of at most 255 bytes")
	}
	if _, _, err := mime.ParseMediaType(e.MediaType); err != nil || !strings.Contains(e.MediaType, "/") {
		return fail("media_type is invalid")
	}
	if e.Size < 0 || e.Docbank.NodeID <= 0 {
		return fail("size or node_id is invalid")
	}
	if !attachmentUUID.MatchString(e.Docbank.VaultUID) || !attachmentUUID.MatchString(e.Docbank.ContentVersionID) || !attachmentHash.MatchString(e.Docbank.ContentSHA256) {
		return fail("Docbank identity is invalid")
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 2048 {
		return fail("entry exceeds 2 KiB")
	}
	return nil
}

// Add never edits an existing entry; callers resolve idempotent retries separately.
func (m Attachments) Add(id string, e Attachment) error {
	if err := ValidateEntry(id, e); err != nil {
		return err
	}
	if _, ok := m[id]; ok {
		return fmt.Errorf("%w: attachment id already exists", ErrInvalidValue)
	}
	count := 0
	for key := range m {
		if _, err := m.Lookup(key); err == nil {
			count++
		}
	}
	if count >= 100 {
		return fmt.Errorf("%w: at most 100 attachments", ErrInvalidValue)
	}
	raw, err := json.Marshal(e)
	if err == nil {
		m[id] = raw
	}
	return err
}

// Remove permits deleting invalid and over-limit peer entries. Empty maps encode as {}.
func (m Attachments) Remove(id string) { delete(m, id) }

// Lookup revalidates each entry at use, without changing its stored representation.
func (m Attachments) Lookup(id string) (Attachment, error) {
	var e Attachment
	raw, ok := m[id]
	if !ok {
		return e, fmt.Errorf("attachment not found")
	}
	if len(raw) > 2048 {
		return e, fmt.Errorf("%w: attachment entry exceeds 2 KiB", ErrInvalidValue)
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("%w: attachment: %v", ErrInvalidValue, err)
	}
	return e, ValidateEntry(id, e)
}
