package jsonl_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func TestExportWritesOrderedRecordsWithSequenceLast(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	attachAlias(ctx, t, d, p.ID, "github.com/wesm/kata", "git", "/tmp/kata")
	issue := createTesterIssue(ctx, t, d, p.ID, "export me", "", "bug")
	addTesterComment(ctx, t, d, issue.ID, "jsonl comment")

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})

	require.NotEmpty(t, records)
	assert.Equal(t, "meta", records[0]["kind"])
	assert.Equal(t, map[string]any{"key": "export_version", "value": "11"}, records[0]["data"])
	assert.Equal(t, "sqlite_sequence", records[len(records)-1]["kind"])

	assertKindOrder(t, records)
}

func TestExportEmitsEventPayloadAsJSONObject(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:      p.ID,
		Title:          "payload",
		Author:         "tester",
		IdempotencyKey: "abc",
	})
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})

	var found bool
	for _, rec := range records {
		if rec["kind"] != "event" {
			continue
		}
		data := rec["data"].(map[string]any)
		payload, ok := data["payload"].(map[string]any)
		require.True(t, ok, "payload should be a JSON object, got %T", data["payload"])
		assert.Equal(t, "abc", payload["idempotency_key"])
		found = true
	}
	assert.True(t, found, "expected at least one event record")
}

func TestExportLegacyV1OmitsUIDFields(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyV1DB(t, path)
	d, err := db.OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})

	assert.Equal(t, map[string]any{"key": "export_version", "value": "1"}, records[0]["data"])
	for _, rec := range records {
		data, _ := rec["data"].(map[string]any)
		switch rec["kind"] {
		case "project", "issue":
			assert.NotContains(t, data, "uid")
		case "link":
			assert.NotContains(t, data, "from_issue_uid")
			assert.NotContains(t, data, "to_issue_uid")
		case "event":
			assert.NotContains(t, data, "issue_uid")
			assert.NotContains(t, data, "related_issue_uid")
		case "purge_log":
			assert.NotContains(t, data, "issue_uid")
			assert.NotContains(t, data, "project_uid")
		}
	}
}

func TestExportProjectIDFiltersProjectScopedRows(t *testing.T) {
	ctx := context.Background()
	d := openExportTestDB(t)
	p1, err := d.CreateProject(ctx, "kata")
	require.NoError(t, err)
	p2, err := d.CreateProject(ctx, "other")
	require.NoError(t, err)
	attachAlias(ctx, t, d, p1.ID, "github.com/wesm/kata", "git", "/tmp/kata")
	attachAlias(ctx, t, d, p2.ID, "github.com/wesm/other", "git", "/tmp/other")
	createTesterIssue(ctx, t, d, p1.ID, "keep me", "")
	createTesterIssue(ctx, t, d, p2.ID, "drop me", "")

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{
		ProjectID:      p1.ID,
		IncludeDeleted: true,
	})

	assertRecordsDoNotContain(t, records, "drop me")
	assertProjectIDs(t, records, map[int64]bool{p1.ID: true})
}

func TestExportNoIncludeDeletedOmitsSoftDeletedIssueDependents(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	kept := createTesterIssue(ctx, t, d, p.ID, "kept issue", "")
	deleted := createTesterIssue(ctx, t, d, p.ID, "deleted issue", "", "gone")
	addTesterComment(ctx, t, d, deleted.ID, "deleted comment")
	_, _, err := d.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		ProjectID:   p.ID,
		FromIssueID: deleted.ID,
		ToIssueID:   kept.ID,
		Type:        "blocks",
		Author:      "tester",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: deleted.ID,
		FromShortID:  deleted.ShortID, FromUID: deleted.UID,
		ToShortID: kept.ShortID, ToUID: kept.UID,
		Actor: "tester",
	})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE issues SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, deleted.ID)
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: false})

	assertRecordsDoNotContain(t, records, "deleted issue")
	assertRecordsDoNotContain(t, records, "deleted comment")
	assertRecordsDoNotContain(t, records, "gone")
	for _, rec := range records {
		data, _ := rec["data"].(map[string]any)
		if rec["kind"] == "link" {
			assert.NotEqual(t, float64(deleted.ID), data["from_issue_id"])
			assert.NotEqual(t, float64(deleted.ID), data["to_issue_id"])
		}
		if rec["kind"] == "event" {
			assert.NotEqual(t, float64(deleted.ID), data["issue_id"])
			assert.NotEqual(t, float64(deleted.ID), data["related_issue_id"])
		}
	}
}

// TestExportNoIncludeDeletedNullsAggregatedEnvelopePeerOnSoftDelete
// pins the round-trip property for live-only exports of single-peer
// aggregated events: when iteration-16's envelope-peer fix sets
// related_issue_id pointing at a now-soft-deleted peer, the live-only
// export must emit NULL for that FK because the peer's row is
// intentionally omitted from the export. Without this scrub, the
// importer would re-insert the FK and fail on the dangling reference.
// The payload's *_uids slices retain the orphan UID per kata#1's
// preservation rule — the wire FK alone is sanitized.
func TestExportNoIncludeDeletedNullsAggregatedEnvelopePeerOnSoftDelete(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	subject := createTesterIssue(ctx, t, d, p.ID, "subject", "")
	target := createTesterIssue(ctx, t, d, p.ID, "target", "")

	_, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   subject.ID,
		Actor:     "tester",
		AddBlocks: []int64{target.ID},
	})
	require.NoError(t, err)

	_, err = d.ExecContext(ctx, `UPDATE issues SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, target.ID)
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: false})

	var aggregated map[string]any
	for _, rec := range records {
		if rec["kind"] != "event" {
			continue
		}
		data, _ := rec["data"].(map[string]any)
		if data["type"] == "issue.links_changed" {
			aggregated = data
			break
		}
	}
	require.NotNil(t, aggregated, "expected the aggregated event to survive in the export")
	assert.Nil(t, aggregated["related_issue_id"],
		"live-only export must NULL related_issue_id when peer is soft-deleted")
	assert.Nil(t, aggregated["related_issue_uid"],
		"live-only export must NULL related_issue_uid when peer is soft-deleted")
	bs, _ := json.Marshal(aggregated["payload"])
	assert.Contains(t, string(bs), target.UID,
		"payload must keep the orphan UID for historical context")
}

// TestExportNoIncludeDeletedPreservesSinglePeerAggregatedEvent pins
// export consistency for aggregated issue.links_changed events: the
// iteration-16 envelope-peer fix sets related_issue_id for single-peer
// edits, but the live-only export filter must NOT drop them on peer
// soft-delete. Erasing single-peer events while preserving multi-peer
// events would make exported history depend on edit batch size, which
// is just as wrong as the broader history-loss problem.
func TestExportNoIncludeDeletedPreservesSinglePeerAggregatedEvent(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	subject := createTesterIssue(ctx, t, d, p.ID, "subject", "")
	target := createTesterIssue(ctx, t, d, p.ID, "target", "")

	_, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   subject.ID,
		Actor:     "tester",
		AddBlocks: []int64{target.ID},
	})
	require.NoError(t, err)

	_, err = d.ExecContext(ctx, `UPDATE issues SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, target.ID)
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: false})

	var found bool
	for _, rec := range records {
		if rec["kind"] != "event" {
			continue
		}
		data, _ := rec["data"].(map[string]any)
		if data["type"] != "issue.links_changed" {
			continue
		}
		bs, err := json.Marshal(data["payload"])
		require.NoError(t, err)
		if assert.Contains(t, string(bs), target.UID,
			"single-peer aggregated event must survive peer soft-delete in live-only export") {
			found = true
		}
	}
	assert.True(t, found, "expected the single-peer aggregated issue.links_changed event to be exported")
}

// TestExportNoIncludeDeletedPreservesLinksChangedReferencingDeleted
// pins Jesse's design call on kata#1: the live-only export of a
// surviving issue must keep its mutation events intact even when the
// payload references a now-soft-deleted peer. Erasing that history
// would lose the context that the surviving issue was once linked to
// the soft-deleted peer. The export filter only drops events whose
// issue_id / related_issue_id refer to a soft-deleted issue; payload
// references are exported with their orphan UIDs intact.
func TestExportNoIncludeDeletedPreservesLinksChangedReferencingDeleted(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	subject := createTesterIssue(ctx, t, d, p.ID, "subject", "")
	target := createTesterIssue(ctx, t, d, p.ID, "target", "")
	// Multi-peer edit so the aggregated event's envelope related_issue_id
	// stays NULL — otherwise iteration-16 sets it to target and the
	// existing related_issue_id filter drops the event on its own.
	other := createTesterIssue(ctx, t, d, p.ID, "other peer", "")

	_, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   subject.ID,
		Actor:     "tester",
		AddBlocks: []int64{target.ID, other.ID},
	})
	require.NoError(t, err)

	_, err = d.ExecContext(ctx, `UPDATE issues SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, target.ID)
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: false})

	var found bool
	for _, rec := range records {
		if rec["kind"] != "event" {
			continue
		}
		data, _ := rec["data"].(map[string]any)
		if data["type"] != "issue.links_changed" {
			continue
		}
		bs, err := json.Marshal(data["payload"])
		require.NoError(t, err)
		if assert.Contains(t, string(bs), target.UID,
			"issue.links_changed event must preserve its peer reference even after soft-delete") {
			found = true
		}
	}
	assert.True(t, found, "expected an exported issue.links_changed event referencing the soft-deleted peer")
}

// TestExportNoIncludeDeletedPreservesNonAggregatedRelatedOrphan: a
// non-issue.links_changed event whose related_issue_id points at a
// fully-missing peer (orphan FK, not soft-delete) must survive a
// live-only export with NULL related fields, mirroring the fix for
// issue #43 where preflight classifies these as scrub. The pre-fix
// WHERE clause dropped them outright because EXISTS-of-live-peer
// failed for both soft-deleted and hard-missing peers; the fix
// switched to NOT-EXISTS-of-soft-deleted-peer so missing peers fall
// through to the SELECT-side CASE scrub.
func TestExportNoIncludeDeletedPreservesNonAggregatedRelatedOrphan(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	subject := createTesterIssue(ctx, t, d, p.ID, "subject", "")

	// Insert an issue.linked event (NOT issue.links_changed) whose
	// related_issue_id points at an issue that does not exist. We
	// flip foreign_keys=OFF for the insert because the FK on
	// events.related_issue_id would otherwise reject it. SQLite's
	// foreign_keys pragma is connection-local, so pin every step to
	// one *sql.Conn and restore the pragma before returning the
	// connection to the pool — otherwise a later test could check
	// out a connection still in FK-off state.
	conn, err := d.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()
	_, err = conn.ExecContext(ctx,
		`INSERT INTO events (uid, origin_instance_uid, project_id, project_name,
		                     issue_id, issue_uid, related_issue_id, related_issue_uid,
		                     type, actor, payload)
		 VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', ?, ?,
		         ?, ?, 999, '01HZZZZZZZZZZZZZZZZZZZZA99',
		         'issue.linked', 'tester', '{}')`,
		"01HZZZZZZZZZZZZZZZZZRELOR1", p.ID, p.Name, subject.ID, subject.UID)
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: false})

	var orphan map[string]any
	for _, rec := range records {
		if rec["kind"] != "event" {
			continue
		}
		data, _ := rec["data"].(map[string]any)
		if data["type"] == "issue.linked" {
			orphan = data
			break
		}
	}
	require.NotNil(t, orphan,
		"non-aggregated event with orphan related_issue_id must survive live-only export")
	assert.Nil(t, orphan["related_issue_id"],
		"orphan related_issue_id must be NULL-scrubbed in the exported event")
	assert.Nil(t, orphan["related_issue_uid"],
		"orphan related_issue_uid must be NULL-scrubbed in the exported event")
}

func openExportTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// newExportEnv opens a fresh test DB and seeds the canonical "kata" project
// used by most export tests.
func newExportEnv(t *testing.T) (context.Context, *db.DB, db.Project) {
	t.Helper()
	ctx := context.Background()
	d := openExportTestDB(t)
	p, err := d.CreateProject(ctx, "kata")
	require.NoError(t, err)
	return ctx, d, p
}

// exportAndDecode runs jsonl.Export into a buffer and decodes the resulting
// JSONL stream into records.
func exportAndDecode(ctx context.Context, t *testing.T, d *db.DB, opts jsonl.ExportOptions) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, d, &out, opts))
	return decodeJSONLLines(t, out.Bytes())
}

func assertRecordsDoNotContain(t *testing.T, records []map[string]any, needle string) {
	t.Helper()
	for _, rec := range records {
		bs, err := json.Marshal(rec)
		require.NoError(t, err)
		assert.NotContains(t, string(bs), needle)
	}
}

func assertProjectIDs(t *testing.T, records []map[string]any, allowed map[int64]bool) {
	t.Helper()
	for _, rec := range records {
		data, _ := rec["data"].(map[string]any)
		v, ok := data["project_id"]
		if !ok {
			if rec["kind"] == "project" {
				v = data["id"]
			} else {
				continue
			}
		}
		id := int64(v.(float64))
		assert.True(t, allowed[id], "record kind=%s has project id %d outside filter", rec["kind"], id)
	}
}

func decodeJSONLLines(t *testing.T, bs []byte) []map[string]any {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(bs))
	var out []map[string]any
	for scanner.Scan() {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
		out = append(out, rec)
	}
	require.NoError(t, scanner.Err())
	return out
}

func assertKindOrder(t *testing.T, records []map[string]any) {
	t.Helper()
	order := map[string]int{
		"meta": 0, "project": 1, "project_alias": 2, "recurrence": 3,
		"issue": 4, "comment": 5, "issue_label": 6, "link": 7,
		"import_mapping": 8, "event": 9, "purge_log": 10, "sqlite_sequence": 11,
	}
	last := -1
	for _, rec := range records {
		kind := rec["kind"].(string)
		rank, ok := order[kind]
		require.True(t, ok, "unknown kind %q", kind)
		require.GreaterOrEqual(t, rank, last, "kind %q out of order", kind)
		last = rank
	}
}
