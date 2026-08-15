package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/similarity"
)

// Fingerprint returns the lowercase hex SHA-256 of the canonical concatenation
// of (title, body, owner, sorted labels, sorted links, priority) per spec §3.6.
// The fingerprint is order-independent for labels and links: both are sorted
// before hashing. Owner is canonicalized as "" when nil or empty. Labels are
// alphabetized. Links are sorted by (type, to_number).
//
// Canonical byte layout (the input to SHA-256):
//
//	title=<canonical-title>\nbody=<canonical-body>\nowner=<canonical-owner>\nlabels=<csv-of-sorted-labels>\nlinks=<canonical-json>
//
// When priority is non-nil, an extra "\npriority=<N>" line is appended after
// the links line. Nil priority emits no priority line so the canonical layout
// matches pre-priority fingerprints byte-for-byte; existing idempotency events
// stored against the five-line layout continue to match.
//
// When metadata is non-empty, a trailing "\nmetadata=<canonical-json>" line is
// appended (after the optional priority line). The metadata JSON is a sorted
// array of {"key":…,"value":…} records whose values are normalized JSON
// (deterministic key order, insignificant whitespace stripped). Empty or nil
// metadata emits NO metadata line, so requests without metadata hash
// byte-for-byte identically to fingerprints stored before metadata joined the
// signature — no legacy fallback machinery is needed.
//
// where canonical-* applies similarity.Canonical (NFC + trim + collapse internal
// whitespace, case preserved). Cross-language clients reproducing this must use
// the same line layout, sort labels alphabetically, sort links by
// (type, to_number), and emit links as the JSON shape
// `[{"type":"…","other_number":N},…]`.
//
// Label-charset assumption: labels are constrained at the API layer to
// `[a-z0-9._:-]` (see the labels CHECK constraint in schema.sql), so the `,`
// separator can never collide with a label byte. Bypassing API validation
// before calling Fingerprint may break this contract.
func Fingerprint(title, body string, owner *string, labels []string, links []InitialLink, priority *int64, metadata map[string]json.RawMessage) string {
	return fingerprintCore(title, body, owner, labels, DedupeLinks(links), priority, metadata)
}

// FingerprintLegacy reproduces the pre-kata#1 hashing layout that did NOT
// dedupe links before sort + serialize. Lookup paths compute both forms so
// idempotency events written before the dedupe-in-Fingerprint change still
// match a retry under the new code. New writes always use Fingerprint
// (deduped); FingerprintLegacy is read-only at the lookup boundary.
func FingerprintLegacy(title, body string, owner *string, labels []string, links []InitialLink, priority *int64, metadata map[string]json.RawMessage) string {
	// Pass links through unchanged so the canonical form preserves any
	// duplicate / Incoming=true entries the caller emitted at create time.
	// The metadata section is layout-identical to Fingerprint: it is omitted
	// when empty, so legacy compatibility for metadata-free requests is
	// preserved automatically and only the link layout differs.
	return fingerprintCore(title, body, owner, labels, append([]InitialLink(nil), links...), priority, metadata)
}

func fingerprintCore(title, body string, owner *string, labels []string, sortedLinks []InitialLink, priority *int64, metadata map[string]json.RawMessage) string {
	ownerStr := ""
	if owner != nil {
		ownerStr = *owner
	}
	sortedLabels := append([]string(nil), labels...)
	sort.Strings(sortedLabels)
	sort.Slice(sortedLinks, func(i, j int) bool {
		if sortedLinks[i].Type != sortedLinks[j].Type {
			return sortedLinks[i].Type < sortedLinks[j].Type
		}
		if sortedLinks[i].ToNumber != sortedLinks[j].ToNumber {
			return sortedLinks[i].ToNumber < sortedLinks[j].ToNumber
		}
		// Incoming is part of the sort key because (blocks, N, false) and
		// (blocks, N, true) describe distinct requests (--blocks vs
		// --blocked-by). Without this discriminator, retried creates with
		// the same idempotency key but flipped direction would silently
		// reuse the wrong issue.
		return !sortedLinks[i].Incoming && sortedLinks[j].Incoming
	})
	// Use a fixed JSON form for the links portion so cross-language clients
	// can reproduce the same bytes. Each entry is {"type":"…","other_number":N}
	// per spec §3.6, plus an optional "incoming":true tail when the link is
	// inverse-direction (blocked_by). incoming=false uses omitempty so
	// pre-Incoming fingerprints continue to match byte-for-byte for the
	// common outgoing case.
	type linkRec struct {
		Type        string `json:"type"`
		OtherNumber int64  `json:"other_number"`
		Incoming    bool   `json:"incoming,omitempty"`
	}
	linkRecs := make([]linkRec, 0, len(sortedLinks))
	for _, l := range sortedLinks {
		linkRecs = append(linkRecs, linkRec{Type: l.Type, OtherNumber: l.ToNumber, Incoming: l.Incoming})
	}
	linksJSON, _ := json.Marshal(linkRecs) // never errors on this shape

	var b strings.Builder
	b.WriteString("title=")
	b.WriteString(similarity.Canonical(title))
	b.WriteString("\nbody=")
	b.WriteString(similarity.Canonical(body))
	b.WriteString("\nowner=")
	b.WriteString(similarity.Canonical(ownerStr))
	b.WriteString("\nlabels=")
	b.WriteString(strings.Join(sortedLabels, ","))
	b.WriteString("\nlinks=")
	b.WriteString(similarity.Canonical(string(linksJSON)))
	if priority != nil {
		fmt.Fprintf(&b, "\npriority=%d", *priority)
	}
	// Only emit the metadata section when metadata is non-empty so that
	// requests without metadata produce byte-identical fingerprints to those
	// stored before metadata joined the signature. See canonicalMetadata.
	if len(metadata) > 0 {
		b.WriteString("\nmetadata=")
		b.WriteString(canonicalMetadata(metadata))
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// canonicalMetadata renders a metadata blob to a deterministic JSON string:
// keys sorted, each value passed through metadata.NormalizeJSON so semantically
// equal values (whitespace, key order within nested objects) collapse to
// identical bytes while distinct numeric literals (including integers beyond
// 2^53) stay distinct. Serialized as an array of {key,value} records
// rather than a JSON object so the ordering is explicit and not dependent on
// any encoder's map-key behavior for the outer shape.
func canonicalMetadata(md map[string]json.RawMessage) string {
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type metaRec struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	recs := make([]metaRec, 0, len(keys))
	for _, k := range keys {
		recs = append(recs, metaRec{Key: k, Value: json.RawMessage(metadata.NormalizeJSON(md[k]))})
	}
	out, _ := json.Marshal(recs) // never errors: values are pre-normalized JSON
	return string(out)
}

// DedupeLinks collapses duplicates from the provided InitialLink slice using
// the same key the schema's UNIQUE constraint enforces. The "blocks" type is
// directional so Incoming is part of the key; "related" is symmetric, so we
// normalize Incoming to false before keying to avoid two semantically identical
// entries surviving dedupe.
//
// For type=related the link is symmetric and canonical-ordered by storage,
// so an inbound and outbound entry for the same target produce the same
// row. We normalize Incoming → false for related entries before keying so
// (related, 5, false) and (related, 5, true) collapse to one — without
// this, the second insert would hit the schema's UNIQUE and surface as
// a 500 instead of the documented no-op.
func DedupeLinks(in []InitialLink) []InitialLink {
	type key struct {
		Type     string
		ToNumber int64
		Incoming bool
	}
	seen := make(map[key]struct{}, len(in))
	out := make([]InitialLink, 0, len(in))
	for _, l := range in {
		normalized := l
		if l.Type == "related" {
			normalized.Incoming = false
		}
		k := key{Type: normalized.Type, ToNumber: normalized.ToNumber, Incoming: normalized.Incoming}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, normalized)
	}
	return out
}
