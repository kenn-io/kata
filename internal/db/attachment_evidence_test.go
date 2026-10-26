package db

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode"
	"uuid"

	"go.kenn.io/kata/internal/metadata"
	"hegel.dev/go/hegel"
)

// Acceptance follows the documented reference shape, including opaque future
// formats being rejected, rather than trusting a caller-provided snapshot.
func TestAttachmentEvidenceResolvesExactlyValidReferences(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		media := hegel.Draw(ht, hegel.Text())
		size := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		n := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		version := uuid.MustParse(fmt.Sprintf("6f1c2e8a-2222-4222-8222-%012x", n&0xffffffffffff))
		if hegel.Draw(ht, hegel.Booleans()) {
			version = uuid.Nil()
		}
		missingSize := hegel.Draw(ht, hegel.Booleans())
		unknown := hegel.Draw(ht, hegel.Booleans())
		raw := map[string]any{"name": name, "media_type": media, "size": size, "docbank_version": version}
		if missingSize {
			delete(raw, "size")
		}
		if unknown {
			raw["future_field"] = true
		}
		encoded, err := json.Marshal(map[string]any{"attachment.example": raw})
		if err != nil {
			ht.Fatal(err)
		}
		evidence := []Evidence{{Type: "attachment", AttachmentID: "example", Attachment: &metadata.Attachment{Name: "forged.pdf"}}}
		out, err := ResolveAttachmentEvidence(Issue{Metadata: JSONBlob(encoded)}, evidence)
		validName := name != "" && len(name) <= 255 && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsFunc(name, unicode.IsControl)
		valid := validName && media != "" && size >= 0 && version != uuid.Nil() && !missingSize && !unknown
		if (err == nil) != valid {
			ht.Fatalf("valid=%t raw=%v err=%v", valid, raw, err)
		}
		if valid {
			expected := metadata.Attachment{Name: name, MediaType: media, Size: size, DocbankVersion: version}
			if len(out) != 1 || out[0].Attachment == nil || *out[0].Attachment != expected {
				ht.Fatalf("snapshot mismatch: %+v expected %+v", out, expected)
			}
			out[0].Attachment.Name = "edited.pdf"
			if evidence[0].Attachment.Name != "forged.pdf" {
				ht.Fatal("enrichment aliased caller evidence")
			}
		}
	}, hegel.WithTestCases(2000))
}
