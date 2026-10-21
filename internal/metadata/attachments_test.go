package metadata

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const attachmentID = "01JB3Z6V2K8M4Q9T1X5W7Y0RNA"

func validAttachment() Attachment {
	return Attachment{Name: "screen.png", MediaType: "image/png", Size: 3, AddedBy: "operator", AddedAt: "2026-10-20T23:10:00Z", Docbank: DocbankReference{VaultUID: "6f1c2e8a-1111-4111-8111-111111111111", NodeID: 42, ContentVersionID: "6f1c2e8a-2222-4222-8222-222222222222", ContentSHA256: strings.Repeat("a", 64)}}
}
func TestAttachmentsShape(t *testing.T) {
	for _, raw := range []string{`{}`, `{"` + attachmentID + `":{}}`, `null`} {
		require.NoError(t, Validate(IssueRegistry, "attachments", jsontext.Value(raw)))
	}
	for _, raw := range []string{`[]`, `1`, `"x"`, `{"bad":{}}`, `{"` + attachmentID + `":null}`, `{"` + attachmentID + `":[]}`} {
		require.ErrorIs(t, Validate(IssueRegistry, "attachments", jsontext.Value(raw)), ErrInvalidValue, raw)
	}
}
func TestAttachmentAdmission(t *testing.T) {
	require.NoError(t, ValidateEntry(attachmentID, validAttachment()))
	cases := map[string]func(*Attachment){"empty name": func(e *Attachment) { e.Name = "" }, "long name": func(e *Attachment) { e.Name = strings.Repeat("a", 256) }, "path": func(e *Attachment) { e.Name = "../file" }, "backslash": func(e *Attachment) { e.Name = `a\b` }, "control": func(e *Attachment) { e.Name = "a\n" }, "mime": func(e *Attachment) { e.MediaType = "wrong" }, "size": func(e *Attachment) { e.Size = -1 }, "node": func(e *Attachment) { e.Docbank.NodeID = 0 }, "vault": func(e *Attachment) { e.Docbank.VaultUID = "x" }, "version": func(e *Attachment) { e.Docbank.ContentVersionID = "x" }, "hash": func(e *Attachment) { e.Docbank.ContentSHA256 = strings.Repeat("A", 64) }, "entry limit": func(e *Attachment) { e.AddedBy = strings.Repeat("a", 2048) }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := validAttachment()
			change(&e)
			require.Error(t, ValidateEntry(attachmentID, e))
		})
	}
	require.Error(t, ValidateEntry("bad", validAttachment()))
	m := Attachments{}
	for i := range 100 {
		raw, err := json.Marshal(validAttachment())
		require.NoError(t, err)
		m[fmt.Sprintf("%026d", i)] = raw
	}
	require.Error(t, m.Add(attachmentID, validAttachment()))
	m.Remove("00000000000000000000000000")
	require.NoError(t, m.Add(attachmentID, validAttachment()))
	require.Error(t, m.Add(attachmentID, validAttachment()))
}

// Contract: local changes must not discard opaque peer fields or over-limit maps.
func TestAttachmentsPreservePeers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(0, 99).Draw(t, "count")
		value := rapid.String().Draw(t, "opaque")
		m := Attachments{}
		raw, _ := json.Marshal(map[string]any{"future": value, "unknown": []int{1, 2, 3}})
		for i := range count {
			m[fmt.Sprintf("%026d", i)] = bytes.Clone(raw)
		}
		before := m.Encode()
		require.NoError(t, m.Add(attachmentID, validAttachment()))
		encoded := m.Encode()
		got, err := DecodeAttachments(encoded)
		require.NoError(t, err)
		for id := range m {
			if id != attachmentID {
				require.Equal(t, raw, []byte(got[id]))
			}
		}
		m.Remove(attachmentID)
		require.Equal(t, before, m.Encode())
	})
}
func TestAttachmentLookupAndLastRemoval(t *testing.T) {
	m := Attachments{}
	require.NoError(t, m.Add(attachmentID, validAttachment()))
	e, err := m.Lookup(attachmentID)
	require.NoError(t, err)
	require.Equal(t, validAttachment(), e)
	m.Remove(attachmentID)
	require.Equal(t, `{}`, string(m.Encode()))
	m[attachmentID] = jsontext.Value(`{"future":true}`)
	_, err = m.Lookup(attachmentID)
	require.Error(t, err)
}

func TestAttachmentLookupRejectsUnknownFields(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			raw, err := json.Marshal(validAttachment())
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(raw, &fields))
			if nested {
				fields["docbank"].(map[string]any)["future"] = true
			} else {
				fields["future"] = true
			}
			raw, err = json.Marshal(fields)
			require.NoError(t, err)
			m := Attachments{attachmentID: raw}
			_, err = m.Lookup(attachmentID)
			require.ErrorIs(t, err, ErrInvalidValue)
			require.JSONEq(t, string(raw), string(m[attachmentID]))
		})
	}
}

func TestAttachmentsOverLimitPeersDecodeAndRemove(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(100, 150).Draw(t, "count")
		raw, err := json.Marshal(validAttachment())
		require.NoError(t, err)
		peers := Attachments{}
		for i := range count {
			peers[fmt.Sprintf("%026d", i)] = bytes.Clone(raw)
		}
		got, err := DecodeAttachments(peers.Encode())
		require.NoError(t, err)
		require.Error(t, got.Add(attachmentID, validAttachment()))
		got.Remove(fmt.Sprintf("%026d", 0))
		require.Len(t, got, count-1)
		for id, entry := range got {
			require.Equal(t, peers[id], entry)
		}
	})
}

func TestAttachmentsInvalidPeersDoNotCountTowardLimit(t *testing.T) {
	m := Attachments{}
	for i := range 100 {
		m[fmt.Sprintf("%026d", i)] = jsontext.Value(`{}`)
	}
	require.NoError(t, m.Add(attachmentID, validAttachment()))
	entry, err := m.Lookup(attachmentID)
	require.NoError(t, err)
	require.Equal(t, "screen.png", entry.Name)
}
