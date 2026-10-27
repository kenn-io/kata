package db

import (
	"encoding/json/v2"
	"maps"
	"testing"
	"uuid"

	"go.kenn.io/kata/internal/metadata"
)

// Acceptance follows the documented reference shape, including opaque future
// formats being rejected, rather than trusting a caller-provided snapshot.
func TestAttachmentEvidenceResolvesExactlyValidReferences(t *testing.T) {
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222")
	valid := map[string]any{"name": "report.pdf", "media_type": "application/pdf", "size": int64(12), "docbank_version": version}
	with := func(key string, value any) map[string]any {
		raw := maps.Clone(valid)
		if value == nil {
			delete(raw, key)
		} else {
			raw[key] = value
		}
		return raw
	}
	for _, tc := range []struct {
		name string
		raw  map[string]any
		ok   bool
	}{
		{"valid", valid, true},
		{"empty size", with("size", int64(0)), true},
		{"path name", with("name", "../report.pdf"), false},
		{"control name", with("name", "report\n.pdf"), false},
		{"empty media type", with("media_type", ""), false},
		{"negative size", with("size", int64(-1)), false},
		{"missing size", with("size", nil), false},
		{"nil version", with("docbank_version", uuid.Nil()), false},
		{"unknown field", with("future_field", true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{"attachment.example": tc.raw})
			if err != nil {
				t.Fatal(err)
			}
			evidence := []Evidence{{Type: "attachment", AttachmentID: "example", Attachment: &metadata.Attachment{Name: "forged.pdf"}}}
			out, err := ResolveAttachmentEvidence(Issue{Metadata: JSONBlob(encoded)}, evidence)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%t err=%v", tc.ok, err)
			}
			if !tc.ok {
				return
			}
			expected := metadata.Attachment{Name: tc.raw["name"].(string), MediaType: tc.raw["media_type"].(string), Size: tc.raw["size"].(int64), DocbankVersion: version}
			if len(out) != 1 || out[0].Attachment == nil || *out[0].Attachment != expected {
				t.Fatalf("snapshot mismatch: %+v expected %+v", out, expected)
			}
			out[0].Attachment.Name = "edited.pdf"
			if evidence[0].Attachment.Name != "forged.pdf" {
				t.Fatal("enrichment aliased caller evidence")
			}
		})
	}
}
