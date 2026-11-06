package metadata

import (
	"encoding/binary"
	"encoding/json/v2"
	"math"
	"testing"
	"uuid"

	"github.com/oklog/ulid/v2"
	"hegel.dev/go/hegel"
)

// All 128 origin identity bits survive the reference codec unchanged. Sizes
// are scalar metadata and need no correspondingly large byte allocation.
func TestAttachmentOriginIdentityRoundTrip(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		var bits ulid.ULID
		binary.BigEndian.PutUint64(bits[:8], hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)))
		binary.BigEndian.PutUint64(bits[8:], hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)))
		input := Attachment{Origin: bits.String(), Name: "file.pdf", MediaType: "application/pdf", Size: hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64)), DocbankVersion: uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222")}
		raw, err := json.Marshal(input)
		if err != nil {
			ht.Fatal(err)
		}
		got, valid := Valid(raw)
		if !valid || got != input {
			ht.Fatalf("origin reference round trip: want %+v, got %+v, valid=%t", input, got, valid)
		}
	}, hegel.WithTestCases(128))
}

func TestAttachmentOriginDoesNotAcceptUnknownMembers(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		key := "extra_" + hegel.Draw(ht, hegel.Text())
		input := map[string]any{"origin": "local", "name": "file.pdf", "media_type": "application/pdf", "size": 0, "docbank_version": "6f1c2e8a-2222-4222-8222-222222222222", key: true}
		raw, err := json.Marshal(input)
		if err != nil {
			ht.Fatal(err)
		}
		if _, valid := Valid(raw); valid {
			ht.Fatalf("unknown member %q accepted in origin reference", key)
		}
	}, hegel.WithTestCases(64))
}
