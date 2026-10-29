package daemon

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"uuid"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"pgregory.net/rapid"
)

// The spec promises one new reference per initially absent version.
func TestAttachmentLinkSequenceAddsEachVersionOnce(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		versions := rapid.SliceOf(rapid.Uint64()).Draw(ht, "versions")
		// Materialize at most 64 operations; retain the full integer/list generators.
		if len(versions) > 64 {
			versions = versions[:64]
		}
		state := jsontext.Value(`{}`)
		expected := map[uuid.UUID]string{}
		for i, n := range versions {
			version := uuid.MustParse(fmt.Sprintf("6f1c2e8a-2222-4222-8222-%012x", n&0xffffffffffff))
			id := fmt.Sprintf("id-%d", i)
			entry := metadata.Attachment{Name: "report.pdf", MediaType: "application/pdf", Size: int64(n >> 1), DocbankVersion: version}
			out, patch, err := attachmentLinkPatch(state, id, entry)
			if err != nil {
				ht.Fatal(err)
			}
			prior, seen := expected[version]
			if seen {
				if out.ID != prior || len(patch) != 0 {
					ht.Fatalf("relink %s changed id or patch: %+v", version, out)
				}
			} else {
				expected[version] = id
				if out.ID != id || len(patch) != 1 {
					ht.Fatalf("new version %s not added: %+v", version, out)
				}
			}
			state, err = db.ApplyMetadataPatch(state, patch)
			if err != nil {
				ht.Fatal(err)
			}
		}
		var values map[string]jsontext.Value
		if err := json.Unmarshal(state, &values); err != nil {
			ht.Fatal(err)
		}
		if len(values) != len(expected) {
			ht.Fatalf("got %d refs for %d versions", len(values), len(expected))
		}
	})
}

// The initial metadata may contain duplicates with different frozen descriptors.
// Reuse must select the smallest valid matching key without rewriting any key.
func TestAttachmentLinkAdversarialInitialMetadata(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		names := rapid.SliceOf(rapid.String()).Draw(ht, "names")
		if len(names) > 32 {
			names = names[:32]
		}
		const version = "6f1c2e8a-2222-4222-8222-222222222222"
		entry := metadata.Attachment{Name: "new.pdf", MediaType: "application/pdf", Size: 12, DocbankVersion: uuid.MustParse(version)}
		values := map[string]jsontext.Value{"custom.note": jsontext.Value(`{"keep":"opaque"}`), "attachment.invalid": jsontext.Value(`{"size":-1,"docbank_version":"` + version + `"}`)}
		expectedID := "new"
		expected := entry
		for i, name := range names {
			// Hex encoding removes separators; overlong names intentionally stay invalid.
			prior := entry
			prior.Name = fmt.Sprintf("file-%x.pdf", []byte(name))
			prior.Size = int64(i)
			id := fmt.Sprintf("old-%02d", i)
			raw, err := json.Marshal(prior)
			if err != nil {
				ht.Fatal(err)
			}
			values[metadata.AttachmentPrefix+id] = raw
			if len(prior.Name) <= 255 && expectedID == "new" {
				expectedID = id
				expected = prior
			}
		}
		raw, err := json.Marshal(values)
		if err != nil {
			ht.Fatal(err)
		}
		out, patch, err := attachmentLinkPatch(raw, "new", entry)
		if err != nil {
			ht.Fatal(err)
		}
		if out.ID != expectedID || out.Attachment != expected {
			ht.Fatalf("got %+v, want %s %+v", out, expectedID, expected)
		}
		if expectedID != "new" && len(patch) != 0 {
			ht.Fatal("existing version produced a patch")
		}
		merged, err := db.ApplyMetadataPatch(raw, patch)
		if err != nil {
			ht.Fatal(err)
		}
		var after map[string]jsontext.Value
		if err := json.Unmarshal(merged, &after); err != nil {
			ht.Fatal(err)
		}
		for key, before := range values {
			if string(after[key]) != string(before) {
				ht.Fatalf("rewrote %s", key)
			}
		}
	})
}
