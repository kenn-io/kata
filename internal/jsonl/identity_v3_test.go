package jsonl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/uid"
)

// TestV2ToV3CutoverFillsIdentity covers spec §8.4: a curated v2 source DB —
// covering events with varied types and an idempotency-key payload, and a
// purge_log row carrying the synthetic purge_reset_after_event_id reservation
// — flows through the cutover and lands at v3 with valid identity columns.
// The reset-cursor reservation is preserved verbatim (a v2-era invariant
// inherited by v3).
func TestV2ToV3CutoverFillsIdentity(t *testing.T) {
	ctx := context.Background()
	d := openCutoverDB(ctx, t, writeLegacyV2DB)

	localUID := fetchInstanceUID(ctx, t, d)
	assert.True(t, uid.Valid(localUID))

	// Every event has a valid uid + origin == new instance, AND the seeded
	// type + payload survive cutover (without a payload check, regressions
	// that drop payloads or rewrite types would pass silently).
	rows, err := d.QueryContext(ctx, `SELECT uid, type, payload, origin_instance_uid FROM events ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var (
		eventCount  int
		seenCreated bool
		seenComment bool
	)
	for rows.Next() {
		var u, typ, payload, origin string
		require.NoError(t, rows.Scan(&u, &typ, &payload, &origin))
		assert.True(t, uid.Valid(u), "event uid %q invalid", u)
		assert.Equal(t, localUID, origin)
		switch typ {
		case "issue.created":
			var p struct {
				IdempotencyKey         string `json:"idempotency_key"`
				IdempotencyFingerprint string `json:"idempotency_fingerprint"`
			}
			require.NoError(t, json.Unmarshal([]byte(payload), &p),
				"issue.created payload not valid JSON after cutover: %s", payload)
			assert.Equal(t, "K1", p.IdempotencyKey,
				"issue.created idempotency_key did not survive cutover")
			assert.Equal(t, "fp", p.IdempotencyFingerprint,
				"issue.created idempotency_fingerprint did not survive cutover")
			seenCreated = true
		case "issue.commented":
			var p struct {
				CommentID int64 `json:"comment_id"`
			}
			require.NoError(t, json.Unmarshal([]byte(payload), &p),
				"issue.commented payload not valid JSON after cutover: %s", payload)
			assert.Equal(t, int64(42), p.CommentID,
				"issue.commented comment_id did not survive cutover")
			seenComment = true
		}
		eventCount++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 2, eventCount, "fixture seeds 2 events (issue.created with idempotency_key + issue.commented)")
	assert.True(t, seenCreated, "issue.created event missing after cutover")
	assert.True(t, seenComment, "issue.commented event missing after cutover")

	// purge_log row also has uid + origin and the reservation is preserved.
	var purgeUID, purgeOrigin string
	var resetAfter int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT uid, origin_instance_uid, purge_reset_after_event_id FROM purge_log WHERE id=1`).
		Scan(&purgeUID, &purgeOrigin, &resetAfter))
	assert.True(t, uid.Valid(purgeUID))
	assert.Equal(t, localUID, purgeOrigin)
	assert.Equal(t, int64(99), resetAfter, "synthetic reset cursor must survive cutover")

	// Re-running the cutover on a fresh copy of the same v2 source must yield
	// identical event/purge_log row UIDs (FromStableSeed determinism), even
	// though instance_uid/origin are intentionally non-deterministic.
	b := openCutoverDB(ctx, t, writeLegacyV2DB)

	for _, q := range []string{
		`SELECT uid FROM events ORDER BY id ASC`,
		`SELECT uid FROM purge_log ORDER BY id ASC`,
	} {
		assert.Equal(t, scanUIDs(t, d, q), scanUIDs(t, b, q), q)
	}
}

// TestV1ToV3CutoverFillsIdentity covers spec §8.5: a v1 source going through
// the cutover path lands at v3 with valid project UIDs, issue UIDs, event UIDs,
// purge_log UIDs, and origin_instance_uid stamped on every event/purge_log row
// matching the new local meta.instance_uid.
func TestV1ToV3CutoverFillsIdentity(t *testing.T) {
	ctx := context.Background()
	d := openCutoverDB(ctx, t, writeLegacyV1DB)

	localUID := fetchInstanceUID(ctx, t, d)
	assert.True(t, uid.Valid(localUID))

	var eventUID, eventOrigin string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT uid, origin_instance_uid FROM events WHERE id=1`).
		Scan(&eventUID, &eventOrigin))
	assert.True(t, uid.Valid(eventUID))
	assert.Equal(t, localUID, eventOrigin)
}

// TestV1ToV3CutoverDeterministicRowUIDs covers spec §8.5: rerunning the cutover
// on the same v1 source produces identical row UIDs (FromStableSeed) for
// projects, issues, events, and purge_log. Note that meta.instance_uid and
// origin_instance_uid columns are intentionally non-deterministic across reruns
// (per §5.3) — those are excluded from the equality check.
func TestV1ToV3CutoverDeterministicRowUIDs(t *testing.T) {
	ctx := context.Background()

	a := openCutoverDB(ctx, t, writeLegacyV1DB)
	b := openCutoverDB(ctx, t, writeLegacyV1DB)

	for _, q := range []string{
		`SELECT uid FROM projects ORDER BY id ASC`,
		`SELECT uid FROM issues ORDER BY id ASC`,
		`SELECT uid FROM events ORDER BY id ASC`,
	} {
		assert.Equal(t, scanUIDs(t, a, q), scanUIDs(t, b, q), q)
	}

	// Sanity: meta.instance_uid is intentionally NOT deterministic across
	// reruns — two clones of the same v1 source must become two distinct
	// installations.
	assert.NotEqual(t, fetchInstanceUID(ctx, t, a), fetchInstanceUID(ctx, t, b))
}

// TestRoundtripV3PreservesInstanceUID covers spec §8.6: a v3 export → v3
// default-mode import preserves meta.instance_uid end-to-end and every event's
// origin_instance_uid still matches the source's identity.
func TestRoundtripV3PreservesInstanceUID(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)
	_, _, err = src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "preserve identity", Author: "tester",
	})
	require.NoError(t, err)

	srcUID := fetchInstanceUID(ctx, t, src)

	buf := exportToBuffer(ctx, t, src)
	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dst))

	assert.Equal(t, srcUID, fetchInstanceUID(ctx, t, dst), "default mode preserves source identity")

	for _, origin := range scanUIDs(t, dst, `SELECT origin_instance_uid FROM events`) {
		assert.Equal(t, srcUID, origin)
	}
}

// TestImportRefreshesCachedInstanceUID guards the default-mode contract that
// a successful import leaves the *sqlitestore.Store handle internally consistent: SQL,
// InstanceUID(), and any subsequent event insert all use the source's
// identity. Without the post-commit refresh the cached field would still hold
// the pre-import LOCAL_FRESH while meta.instance_uid stored the source's
// value, and new events on the same handle would stamp the wrong origin.
func TestImportRefreshesCachedInstanceUID(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	srcUID := src.InstanceUID()

	buf := exportToBuffer(ctx, t, src)

	dst := openImportTargetDB(t)
	preImport := dst.InstanceUID()
	require.NotEqual(t, srcUID, preImport, "fresh target must start with its own identity")

	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dst))
	assert.Equal(t, srcUID, dst.InstanceUID(),
		"default-mode import overwrote meta.instance_uid; cached InstanceUID() must follow")

	// Writing an event on the same handle must use the refreshed value, not
	// the stale LOCAL_FRESH that db.Open originally seeded.
	p, err := dst.CreateProject(ctx, "post-import")
	require.NoError(t, err)
	_, evt, err := dst.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "post-import write", Author: "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, srcUID, evt.OriginInstanceUID,
		"event written after import must inherit refreshed origin_instance_uid")
}

// TestImportNewInstanceRegeneratesIdentity covers spec §8.7: --new-instance
// mode keeps the target's fresh meta.instance_uid (db.Open's value) and
// preserves the source's origin_instance_uid on every imported event.
func TestImportNewInstanceRegeneratesIdentity(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)
	_, _, err = src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "clone me", Author: "tester",
	})
	require.NoError(t, err)

	srcUID := fetchInstanceUID(ctx, t, src)

	buf := exportToBuffer(ctx, t, src)
	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(buf.Bytes()), dst, jsonl.ImportOptions{
		NewInstance: true,
	}))

	dstUID := fetchInstanceUID(ctx, t, dst)
	assert.NotEqual(t, srcUID, dstUID, "new-instance keeps target's fresh identity")
	assert.True(t, uid.Valid(dstUID))

	// Imported events keep the source's origin (loop-detection contract).
	for _, origin := range scanUIDs(t, dst, `SELECT origin_instance_uid FROM events`) {
		assert.Equal(t, srcUID, origin)
	}
}

func scanUIDs(t *testing.T, d *sqlitestore.Store, query string) []string {
	t.Helper()
	rows, err := d.Query(query)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}
