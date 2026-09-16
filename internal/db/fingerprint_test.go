package db_test

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

func TestFingerprint_DeterministicOverInputOrder(t *testing.T) {
	owner := "alice"
	a := db.Fingerprint("fix login", "details", &owner, []string{"bug", "ui"}, []db.InitialLink{{Type: "blocks", ToNumber: 7}, {Type: "parent", ToNumber: 3}}, nil, nil)

	b := db.Fingerprint("fix login", "details", &owner, []string{"ui", "bug"}, []db.InitialLink{{Type: "parent", ToNumber: 3}, {Type: "blocks", ToNumber: 7}}, nil, nil)

	// links reordered
	assert.Equal(t, a, b, "fingerprint must be order-independent for labels and links")
}

func TestFingerprint_CanonicalizesWhitespace(t *testing.T) {
	a := db.Fingerprint("fix login", "body text", nil, nil, nil, nil, nil)
	b := db.Fingerprint("  fix\t\n  login  ", "body  text", nil, nil, nil, nil, nil)
	assert.Equal(t, a, b, "internal whitespace runs and trimming must collapse")
}

// TestFingerprint_BlocksVsBlockedByDiffer pins that --blocks N and
// --blocked-by N produce distinct fingerprints. Without an Incoming
// discriminator in the canonical link record, retried creates with the
// same idempotency key but flipped direction would silently reuse the
// wrong issue.
func TestFingerprint_BlocksVsBlockedByDiffer(t *testing.T) {
	out := db.Fingerprint("fix race", "body", nil, nil, []db.InitialLink{{Type: "blocks", ToNumber: 7, Incoming: false}}, nil, nil)

	in := db.Fingerprint("fix race", "body", nil, nil, []db.InitialLink{{Type: "blocks", ToNumber: 7, Incoming: true}}, nil, nil)

	assert.NotEqual(t, out, in,
		"--blocks N and --blocked-by N must hash differently")
}

// TestFingerprint_OutgoingBlocksByteLayoutStable pins that the
// Incoming=false common case stays byte-for-byte identical to the
// pre-Incoming layout (omitempty drops the field). Pre-Incoming
// idempotency events must continue to match new fingerprints for
// outgoing-only requests.
func TestFingerprint_OutgoingBlocksByteLayoutStable(t *testing.T) {
	defaulted := db.Fingerprint("t", "b", nil, nil, []db.InitialLink{{Type: "blocks", ToNumber: 7}}, nil, nil)

	explicit := db.Fingerprint("t", "b", nil, nil, []db.InitialLink{{Type: "blocks", ToNumber: 7, Incoming: false}}, nil, nil)

	assert.Equal(t, defaulted, explicit,
		"explicit Incoming=false must hash identically to omitted field")
}

// TestFingerprintLegacy_DiffersOnDuplicates pins that the legacy hash
// preserves duplicate link entries (the pre-kata#1 behavior), so the
// daemon's lookup path can match idempotency events that were stored
// before dedupe-in-Fingerprint landed.
func TestFingerprintLegacy_DiffersOnDuplicates(t *testing.T) {
	dup := db.FingerprintLegacy("fix", "b", nil, nil, []db.InitialLink{
		{Type: "related", ToNumber: 2},
		{Type: "related", ToNumber: 2},
	}, nil, nil)

	clean := db.FingerprintLegacy("fix", "b", nil, nil, []db.InitialLink{
		{Type: "related", ToNumber: 2},
	}, nil, nil)

	assert.NotEqual(t, dup, clean,
		"legacy form must hash duplicates differently from the deduped form")
}

// TestFingerprint_DedupesLinksBeforeHashing pins that fingerprint
// canonicalization matches what CreateIssue persists. Without dedupe-
// before-hash, an idempotent retry of `kata create --related 2 --related 2`
// against an existing entry with `--related 2` would trip
// idempotency_mismatch even though the persisted state is identical.
func TestFingerprint_DedupesLinksBeforeHashing(t *testing.T) {
	withDups := db.Fingerprint("fix", "b", nil, nil, []db.InitialLink{
		{Type: "related", ToNumber: 2},
		{Type: "related", ToNumber: 2},
	}, nil, nil)

	clean := db.Fingerprint("fix", "b", nil, nil, []db.InitialLink{
		{Type: "related", ToNumber: 2},
	}, nil, nil)

	assert.Equal(t, withDups, clean,
		"duplicate link entries must canonicalize the same way as the persisted set")

	// related's Incoming=true is normalized to false by DedupeLinks
	// (related is symmetric); the fingerprint must reflect that.
	withInverse := db.Fingerprint("fix", "b", nil, nil, []db.InitialLink{
		{Type: "related", ToNumber: 2},
		{Type: "related", ToNumber: 2, Incoming: true},
	}, nil, nil)

	assert.Equal(t, clean, withInverse,
		"related Incoming=true canonicalizes to the same row as Incoming=false")
}

func TestFingerprint_DiffersOnDifferentInputs(t *testing.T) {
	base := db.Fingerprint("a", "b", nil, nil, nil, nil, nil)
	priority := int64(1)
	owner := "x"
	cases := []struct {
		name        string
		fingerprint string
	}{
		{"different_title", db.Fingerprint("aa", "b", nil, nil, nil, nil, nil)},
		{"different_body", db.Fingerprint("a", "bb", nil, nil, nil, nil, nil)},
		{"different_owner", db.Fingerprint("a", "b", &owner, nil, nil, nil, nil)},
		{"different_labels", db.Fingerprint("a", "b", nil, []string{"bug"}, nil, nil, nil)},
		{"different_links", db.Fingerprint("a", "b", nil, nil, []db.InitialLink{{Type: "blocks", ToNumber: 1}}, nil, nil)},
		{"different_priority", db.Fingerprint("a", "b", nil, nil, nil, &priority, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEqual(t, base, tc.fingerprint)
		})
	}
}

func TestFingerprint_CaseSensitive(t *testing.T) {
	// Spec §3.6: canonical() does NOT lowercase. Title casing matters.
	a := db.Fingerprint("Fix Login", "", nil, nil, nil, nil, nil)
	b := db.Fingerprint("fix login", "", nil, nil, nil, nil, nil)
	assert.NotEqual(t, a, b)
}

func TestFingerprint_NilAndEmptyOwnerAreEquivalent(t *testing.T) {
	empty := ""
	a := db.Fingerprint("a", "b", nil, nil, nil, nil, nil)
	b := db.Fingerprint("a", "b", &empty, nil, nil, nil, nil)
	assert.Equal(t, a, b, "nil owner and empty owner produce the same fingerprint")
}

func TestFingerprint_HexLowercaseSHA256(t *testing.T) {
	got := db.Fingerprint("a", "b", nil, nil, nil, nil, nil)
	assert.Len(t, got, 64, "sha256 hex is 64 chars")
	assert.True(t, strings.ToLower(got) == got, "must be lowercase hex")
}

// TestFingerprint_Vector pins exact hex outputs so any change to the canonical
// byte layout, separator order, JSON shape, sort order, or Canonical()
// behavior immediately breaks the test. This is the cross-language contract.
func TestFingerprint_Vector(t *testing.T) {
	// All-empty inputs: title=\nbody=\nowner=\nlabels=\nlinks=[]
	// Nil priority MUST omit the priority line so this hash matches the
	// pre-priority five-line fingerprint shape — existing idempotency events
	// in older databases continue to match.
	assert.Equal(t,
		"3e3678620b59364a3d56c8608ff431933b042a8619e74892243b0d2bfdb09af2",
		db.Fingerprint("", "", nil, nil, nil, nil, nil),
		"empty-everything fingerprint must not drift")

	// Filled: one label, one parent link.
	assert.Equal(t,
		"2c77531b9b3e7522ccf86eb353fc2aaa8cd8418e1132c8ebb1f2f80ea1dca8db",
		db.Fingerprint("hello", "world", nil, []string{"bug"}, []db.InitialLink{{Type: "parent", ToNumber: 3}}, nil, nil),

		"filled fingerprint must not drift")
}

// TestFingerprint_PriorityNilPreservesLegacyShape locks the rule that nil
// priority emits the same canonical bytes as the pre-priority signature, so
// existing fingerprints stored in databases keep matching after the upgrade.
func TestFingerprint_PriorityNilPreservesLegacyShape(t *testing.T) {
	// Same hash whether priority is nil or omitted entirely.
	a := db.Fingerprint("a", "b", nil, []string{"bug"}, []db.InitialLink{{Type: "parent", ToNumber: 3}}, nil, nil)

	b := db.Fingerprint("a", "b", nil, []string{"bug"}, []db.InitialLink{{Type: "parent", ToNumber: 3}}, nil, nil)

	assert.Equal(t, a, b)

	// Setting any priority diverges from the nil-priority hash.
	zero := int64(0)
	withZero := db.Fingerprint("a", "b", nil, []string{"bug"}, []db.InitialLink{{Type: "parent", ToNumber: 3}}, &zero, nil)

	assert.NotEqual(t, a, withZero,
		"P0 differs from nil priority — they are not the same identity")
}

// TestFingerprint_EmptyMetadataPreservesPreChangeLayout locks the rule that
// nil and empty metadata emit NO metadata section, so identical requests hash
// byte-for-byte the same as fingerprints stored before metadata joined the
// signature. The pinned hex is the pre-change empty-everything vector from
// TestFingerprint_Vector.
func TestFingerprint_EmptyMetadataPreservesPreChangeLayout(t *testing.T) {
	const preChange = "3e3678620b59364a3d56c8608ff431933b042a8619e74892243b0d2bfdb09af2"
	assert.Equal(t, preChange,
		db.Fingerprint("", "", nil, nil, nil, nil, nil),
		"nil metadata must reproduce the pre-change fingerprint")
	assert.Equal(t, preChange,
		db.Fingerprint("", "", nil, nil, nil, nil, map[string]jsontext.Value{}),
		"empty (non-nil) metadata must also omit the metadata section")
}

// TestFingerprint_MetadataChangesFingerprint pins that non-empty metadata
// participates in the identity: a create replay under the same idempotency key
// but different metadata must produce a different fingerprint (→ 409 mismatch),
// while whitespace/key-order-equivalent metadata canonicalizes identically.
func TestFingerprint_MetadataChangesFingerprint(t *testing.T) {
	base := db.Fingerprint("t", "b", nil, nil, nil, nil, nil)
	withMeta := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{"work.branch": jsontext.Value(`"feature/x"`)})
	assert.NotEqual(t, base, withMeta,
		"adding metadata must change the fingerprint")

	changed := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{"work.branch": jsontext.Value(`"feature/y"`)})
	assert.NotEqual(t, withMeta, changed,
		"a different metadata value must change the fingerprint")

	// Insignificant whitespace and nested-object key order must collapse.
	a := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{"work.cfg": jsontext.Value(`{"a":1,"b":2}`)})
	b := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{"work.cfg": jsontext.Value(`{ "b": 2, "a": 1 }`)})
	assert.Equal(t, a, b,
		"whitespace- and key-order-equivalent metadata must hash identically")
}

// TestFingerprint_LargeIntMetadataDistinct pins that metadata values differing
// only by a large integer beyond 2^53 produce different fingerprints. Before the
// canonicalizer preserved integer literals (UseNumber), both values collapsed to
// the same float64 and two distinct idempotency payloads hashed identically,
// making the daemon silently reuse the first issue instead of returning 409.
func TestFingerprint_LargeIntMetadataDistinct(t *testing.T) {
	a := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{"work.n": jsontext.Value(`9223372036854775807`)})
	b := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{"work.n": jsontext.Value(`9223372036854775806`)})
	assert.NotEqual(t, a, b,
		"large-integer metadata values must not collapse to the same fingerprint")
}

// TestFingerprint_MetadataOrderIndependent pins that the map-key insertion
// order of metadata does not affect the hash (keys are sorted canonically).
func TestFingerprint_MetadataOrderIndependent(t *testing.T) {
	a := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{
			"work.branch": jsontext.Value(`"x"`),
			"work.area":   jsontext.Value(`"y"`),
		})
	b := db.Fingerprint("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{
			"work.area":   jsontext.Value(`"y"`),
			"work.branch": jsontext.Value(`"x"`),
		})
	assert.Equal(t, a, b, "metadata fingerprint must be independent of map key order")
}

// TestFingerprintLegacy_MetadataParticipates pins that the legacy form also
// folds metadata into the hash the same way, so the daemon's dual-fingerprint
// lookup treats metadata as identity on both branches.
func TestFingerprintLegacy_MetadataParticipates(t *testing.T) {
	base := db.FingerprintLegacy("t", "b", nil, nil, nil, nil, nil)
	withMeta := db.FingerprintLegacy("t", "b", nil, nil, nil, nil,
		map[string]jsontext.Value{"work.branch": jsontext.Value(`"feature/x"`)})
	assert.NotEqual(t, base, withMeta,
		"legacy fingerprint must also fold in metadata")
}
