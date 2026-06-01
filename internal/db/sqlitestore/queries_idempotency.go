package sqlitestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/similarity"
)

// sqliteTimeFormat matches the schema's strftime('%Y-%m-%dT%H:%M:%fZ', ...)
// (3 fractional-second digits, UTC). Both sides must use the same width for
// SQLite's lexicographic string comparison on created_at to be correct.
const sqliteTimeFormat = "2006-01-02T15:04:05.000Z"

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
func Fingerprint(title, body string, owner *string, labels []string, links []db.InitialLink, priority *int64) string {
	return fingerprintCore(title, body, owner, labels, dedupeLinks(links), priority)
}

// FingerprintLegacy reproduces the pre-kata#1 hashing layout that did NOT
// dedupe links before sort + serialize. Lookup paths compute both forms so
// idempotency events written before the dedupe-in-Fingerprint change still
// match a retry under the new code. New writes always use Fingerprint
// (deduped); FingerprintLegacy is read-only at the lookup boundary.
func FingerprintLegacy(title, body string, owner *string, labels []string, links []db.InitialLink, priority *int64) string {
	// Pass links through unchanged so the canonical form preserves any
	// duplicate / Incoming=true entries the caller emitted at create time.
	return fingerprintCore(title, body, owner, labels, append([]db.InitialLink(nil), links...), priority)
}

func fingerprintCore(title, body string, owner *string, labels []string, sortedLinks []db.InitialLink, priority *int64) string {
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

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// LookupIdempotency searches `events` for an `issue.created` row in the given
// project whose payload's `idempotency_key` equals key and whose created_at is
// at-or-after `since`. Returns nil when no match. Uses the partial index
// idx_events_idempotency declared in 0001_init.sql.
func (d *Store) LookupIdempotency(ctx context.Context, projectID int64, key string, since time.Time) (*db.IdempotencyMatch, error) {
	const q = `
		SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
		       e.issue_id, e.issue_uid,
		       e.related_issue_id, e.related_issue_uid, e.type, e.actor, e.payload,
		       e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at,
		       json_extract(e.payload, '$.idempotency_fingerprint')
		FROM events e
		JOIN projects p ON p.id = e.project_id
		WHERE e.type = 'issue.created'
		  AND e.project_id = ?
		  AND json_extract(e.payload, '$.idempotency_key') = ?
		  AND e.created_at >= ?
		ORDER BY e.id DESC
		LIMIT 1`
	row := d.QueryRowContext(ctx, q, projectID, key, since.UTC().Format(sqliteTimeFormat))

	var (
		evt db.Event
		fp  sql.NullString
	)
	err := row.Scan(&evt.ID, &evt.UID, &evt.OriginInstanceUID, &evt.ProjectID, &evt.ProjectUID, &evt.ProjectName,
		&evt.IssueID, &evt.IssueUID, &evt.RelatedIssueID, &evt.RelatedIssueUID, &evt.Type, &evt.Actor,
		&evt.Payload, &evt.HLCPhysicalMS, &evt.HLCCounter, &evt.ContentHash, &evt.CreatedAt, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup idempotency: %w", err)
	}
	if evt.IssueID == nil {
		// Defensive: an issue.created event without an issue_id is malformed.
		return nil, fmt.Errorf("idempotency match has no issue_id")
	}
	// Carveout (spec §6): idempotency-key collision detection sees the issue
	// even if it has been soft-deleted, so it can report the right mismatch.
	// IssueByID returns rows regardless of deleted_at, matching that intent.
	issue, err := d.IssueByID(ctx, *evt.IssueID)
	if err != nil {
		return nil, fmt.Errorf("idempotency match issue: %w", err)
	}
	return &db.IdempotencyMatch{
		IssueID:      issue.ID,
		IssueShortID: issue.ShortID,
		Fingerprint:  fp.String,
		Event:        evt,
	}, nil
}
