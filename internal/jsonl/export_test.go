package jsonl_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
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
	assert.Equal(t, map[string]any{"key": "export_version", "value": fmt.Sprint(db.CurrentSchemaVersion())}, records[0]["data"])
	assert.Equal(t, "sqlite_sequence", records[len(records)-1]["kind"])

	assertKindOrder(t, records)
}

// TestExportIncludesProjectPurgeLog covers the project_purge_log kind end to end
// through the JSONL export path. The tombstone has no FK to projects, so it must
// be exported even though the project row is gone — and it must sort between
// purge_log and sqlite_sequence.
func TestExportIncludesProjectPurgeLog(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	createTesterIssue(ctx, t, d, p.ID, "doomed", "")
	_, _, err := d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester", Force: true})
	require.NoError(t, err)
	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})
	assertKindOrder(t, records)
	idx := firstKindIndex(records, "project_purge_log")
	require.NotEqual(t, -1, idx, "export must include the project_purge_log tombstone")
	data, ok := records[idx]["data"].(map[string]any)
	require.True(t, ok, "project_purge_log record data should be an object")
	assert.Equal(t, p.Name, data["project_name"])
}

func TestIssueSyncCurrentExportOrderAndProjectFilter(t *testing.T) {
	ctx := context.Background()
	d := openExportTestDB(t)
	project, err := d.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	attachAlias(ctx, t, d, project.ID, "github.com/example-org/example-repo", "git", "/tmp/example-project")
	binding, err := d.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID:       project.ID,
		Provider:        "github",
		SourceKey:       "github:repo-node-example",
		RemoteID:        "repo-node-example",
		DisplayName:     "example-org/example-repo",
		Config:          []byte(`{"host":"github.com","owner":"example-org","repo":"example-repo","repo_id":42}`),
		IntervalSeconds: 900,
	})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		UPDATE issue_sync_bindings
		   SET enabled = 0,
		       last_cursor_at = '2026-06-01T10:00:00.000Z',
		       created_at = '2026-06-01T09:00:00.000Z',
		       updated_at = '2026-06-01T10:01:00.000Z'
		 WHERE id = ?`, binding.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		UPDATE issue_sync_status
		   SET sync_started_at = '2026-06-01T09:58:00.000Z',
		       last_attempt_at = '2026-06-01T09:58:00.000Z',
		       last_success_at = '2026-06-01T10:00:00.000Z',
		       last_error_at = '2026-06-01T10:02:00.000Z',
		       last_error = 'rate limited',
		       last_created = 2,
		       last_updated = 3,
		       last_unchanged = 4,
		       last_comments = 5
		 WHERE binding_id = ?`, binding.ID)
	require.NoError(t, err)
	issue := createTesterIssue(ctx, t, d, project.ID, "example issue", "")

	other, err := d.CreateProject(ctx, "other-example-project")
	require.NoError(t, err)
	_, err = d.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID:       other.ID,
		Provider:        "github",
		SourceKey:       "github:other-repo-node",
		RemoteID:        "other-repo-node",
		DisplayName:     "example-org/other-example-repo",
		Config:          []byte(`{"host":"github.com","owner":"example-org","repo":"other-example-repo","repo_id":43}`),
		IntervalSeconds: 900,
	})
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})
	assertKindOrder(t, records)
	aliasIndex := firstKindIndex(records, "project_alias")
	bindingIndex := firstKindIndex(records, "issue_sync_binding")
	statusIndex := firstKindIndex(records, "issue_sync_status")
	issueIndex := firstKindIndex(records, "issue")
	require.NotEqual(t, -1, aliasIndex)
	require.NotEqual(t, -1, bindingIndex)
	require.NotEqual(t, -1, statusIndex)
	require.NotEqual(t, -1, issueIndex)
	assert.Less(t, aliasIndex, bindingIndex)
	assert.Less(t, bindingIndex, statusIndex)
	assert.Less(t, statusIndex, issueIndex)

	bindingData := records[bindingIndex]["data"].(map[string]any)
	assert.Equal(t, float64(binding.ID), bindingData["id"])
	assert.Equal(t, "github", bindingData["provider"])
	assert.Equal(t, "repo-node-example", bindingData["remote_id"])
	assert.Equal(t, "example-org/example-repo", bindingData["display_name"])
	assert.Equal(t, map[string]any{
		"host":    "github.com",
		"owner":   "example-org",
		"repo":    "example-repo",
		"repo_id": float64(42),
	}, bindingData["config"])
	assert.Equal(t, false, bindingData["enabled"])
	assert.Equal(t, "2026-06-01T10:00:00.000Z", bindingData["last_cursor_at"])

	statusData := records[statusIndex]["data"].(map[string]any)
	assert.Equal(t, float64(binding.ID), statusData["binding_id"])
	assert.Equal(t, "rate limited", statusData["last_error"])
	assert.Equal(t, float64(2), statusData["last_created"])
	assert.Equal(t, float64(3), statusData["last_updated"])
	assert.Equal(t, float64(4), statusData["last_unchanged"])
	assert.Equal(t, float64(5), statusData["last_comments"])

	scoped := exportAndDecode(ctx, t, d, jsonl.ExportOptions{ProjectID: project.ID, IncludeDeleted: true})
	var scopedBindings, scopedStatuses int
	for _, rec := range scoped {
		data, _ := rec["data"].(map[string]any)
		switch rec["kind"] {
		case "issue_sync_binding":
			scopedBindings++
			assert.Equal(t, float64(project.ID), data["project_id"])
			assert.Equal(t, "example-org/example-repo", data["display_name"])
		case "issue_sync_status":
			scopedStatuses++
			assert.Equal(t, float64(project.ID), data["project_id"])
		case "issue":
			assert.Equal(t, float64(issue.ID), data["id"])
		}
	}
	assert.Equal(t, 1, scopedBindings)
	assert.Equal(t, 1, scopedStatuses)
	assertRecordsDoNotContain(t, scoped, "other-example-repo")
}

func TestExportReadOnlyLegacyV17GitHubStatusMapsCounts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	writeLegacyV17GitHubStatusDB(t, path)
	assertLegacyTableExists(t, path, "github_sync_bindings")
	assertLegacyTableExists(t, path, "github_sync_status")
	source, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	var out bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &out, jsonl.ExportOptions{IncludeDeleted: true}))
	records := decodeJSONLLines(t, out.Bytes())

	bindingIndex := firstKindIndex(records, "issue_sync_binding")
	require.NotEqual(t, -1, bindingIndex, "expected legacy github_sync_bindings export")
	bindingData := records[bindingIndex]["data"].(map[string]any)
	assert.Equal(t, "github", bindingData["provider"])
	assert.Equal(t, "github:legacy-repo-node", bindingData["source_key"])
	assert.Equal(t, "legacy-repo-node", bindingData["remote_id"])
	assert.Equal(t, "example-org/legacy-repo", bindingData["display_name"])

	statusIndex := firstKindIndex(records, "issue_sync_status")
	require.NotEqual(t, -1, statusIndex, "expected legacy issue_sync_status export")
	statusData := records[statusIndex]["data"].(map[string]any)
	assert.Equal(t, "rate limited", statusData["last_error"])
	assert.Equal(t, float64(5), statusData["last_comments"],
		"legacy last_comment_count maps to last_comments")
	assert.Equal(t, float64(0), statusData["last_created"],
		"legacy last_issue_count cannot be decomposed; counters reset")
	assert.Equal(t, float64(0), statusData["last_updated"])
	assert.Equal(t, float64(0), statusData["last_unchanged"])
}

func assertLegacyTableExists(t *testing.T, path, table string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	var name string
	err = raw.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	require.NoError(t, err)
	assert.Equal(t, table, name)
}

func TestExportReadOnlyLegacySQLiteUsesVersionAwareExporter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	writeLegacyV1DB(t, path)
	source, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	var out bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &out, jsonl.ExportOptions{IncludeDeleted: true}))
	records := decodeJSONLLines(t, out.Bytes())

	require.NotEmpty(t, records)
	assert.Equal(t, map[string]any{"key": "export_version", "value": "1"}, records[0]["data"])
	assertRecordsContain(t, records, "legacy issue")
	assertRecordsContain(t, records, "legacy comment")
}

func TestExportReadOnlyLegacyV12FederationRowsOmitMissingBoundActor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	writeLegacyV12FederationDB(t, path)
	source, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	var out bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &out, jsonl.ExportOptions{IncludeDeleted: true}))
	records := decodeJSONLLines(t, out.Bytes())

	var sawBinding, sawEnrollment bool
	for _, rec := range records {
		data, ok := rec["data"].(map[string]any)
		require.True(t, ok)
		switch rec["kind"] {
		case "federation_binding":
			sawBinding = true
			assert.NotContains(t, data, "bound_actor")
		case "federation_enrollment":
			sawEnrollment = true
			assert.NotContains(t, data, "bound_actor")
		}
	}
	assert.True(t, sawBinding, "expected legacy federation binding export")
	assert.True(t, sawEnrollment, "expected legacy federation enrollment export")
}

func TestExportReadOnlyLegacyV13FederationRowsPreserveBoundActor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	writeLegacyV13FederationDB(t, path)
	source, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	var out bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &out, jsonl.ExportOptions{IncludeDeleted: true}))
	records := decodeJSONLLines(t, out.Bytes())

	var sawBinding, sawEnrollment bool
	for _, rec := range records {
		data, ok := rec["data"].(map[string]any)
		require.True(t, ok)
		switch rec["kind"] {
		case "federation_binding":
			sawBinding = true
			assert.Equal(t, "legacy-actor", data["bound_actor"])
		case "federation_enrollment":
			sawEnrollment = true
			assert.Equal(t, "legacy-actor", data["bound_actor"])
			assert.NotContains(t, data, "allow_adoption_snapshot_authors")
		}
	}
	assert.True(t, sawBinding, "expected legacy federation binding export")
	assert.True(t, sawEnrollment, "expected legacy federation enrollment export")

	target := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(out.Bytes()), target))
	importedProject, err := target.ProjectByName(ctx, "legacy-fed")
	require.NoError(t, err)
	binding, err := target.FederationBindingByProject(ctx, importedProject.ID)
	require.NoError(t, err)
	assert.True(t, binding.PushEnabled)
	assert.Equal(t, "legacy-actor", binding.Actor)
	var enrollmentCount int
	require.NoError(t, target.QueryRow(`SELECT COUNT(*) FROM federation_enrollments WHERE bound_actor = 'legacy-actor'`).Scan(&enrollmentCount))
	assert.Equal(t, 1, enrollmentCount)
}

func TestExportProjectAliasesOmitPathTelemetry(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	attachAlias(ctx, t, d, p.ID, "github.com/example/project", "git", "/tmp/project")

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})

	var found bool
	for _, rec := range records {
		if rec["kind"] != "project_alias" {
			continue
		}
		found = true
		data, ok := rec["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "github.com/example/project", data["alias_identity"])
		assert.NotContains(t, data, "root_path")
		assert.NotContains(t, data, "last_seen_at")
	}
	assert.True(t, found, "expected project_alias export record")
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
		assert.NotZero(t, data["hlc_physical_ms"])
		assert.NotNil(t, data["hlc_counter"])
		assert.Regexp(t, `^[a-f0-9]{64}$`, data["content_hash"])
		found = true
	}
	assert.True(t, found, "expected at least one event record")
}

func TestExportFederationKindOrderPlacesEnrollmentBeforeEvent(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleHub,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, d.RecordFederationSyncPullStarted(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:00.000Z")))
	require.NoError(t, d.RecordFederationSyncPullSuccess(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:01.000Z")))
	_, err = d.ExecContext(ctx, `
		INSERT INTO federation_enrollments(token_hash, spoke_instance_uid, project_id, capabilities, bound_actor)
		VALUES(?, ?, ?, ?, ?)`,
		strings.Repeat("a", 64), "01HZZZZZZZZZZZZZZZZZZZZZ01", p.ID, "pull,push", "tester")
	require.NoError(t, err)
	createTesterIssue(ctx, t, d, p.ID, "event after federation records", "")

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})

	assertKindOrder(t, records)
	kinds := make([]string, 0, len(records))
	for _, rec := range records {
		kinds = append(kinds, rec["kind"].(string))
	}
	bindingIndex := indexOfKind(kinds, "federation_binding")
	enrollmentIndex := indexOfKind(kinds, "federation_enrollment")
	eventIndex := indexOfKind(kinds, "event")
	require.NotEqual(t, -1, bindingIndex, "expected federation_binding record")
	statusIndex := indexOfKind(kinds, "federation_sync_status")
	require.NotEqual(t, -1, statusIndex, "expected federation_sync_status record")
	require.NotEqual(t, -1, enrollmentIndex, "expected federation_enrollment record")
	require.NotEqual(t, -1, eventIndex, "expected event record")
	assert.Less(t, bindingIndex, enrollmentIndex)
	assert.Less(t, bindingIndex, statusIndex)
	assert.Less(t, statusIndex, enrollmentIndex)
	assert.Less(t, enrollmentIndex, eventIndex)
}

func TestExportFederationSyncStatus(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	// federation_sync_status writers no-op without a live binding (the leave
	// sync-race guard), so seed one before recording sync status.
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleHub,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	err = d.RecordFederationSyncPullStarted(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:00.000Z"))
	require.NoError(t, err)
	err = d.RecordFederationSyncPullSuccess(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:01.000Z"))
	require.NoError(t, err)
	err = d.RecordFederationSyncPushStarted(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:02.000Z"))
	require.NoError(t, err)
	err = d.RecordFederationSyncError(ctx, p.ID, assert.AnError, mustParseTime(t, "2026-05-23T01:00:03.000Z"))
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: true})

	var found map[string]any
	for _, rec := range records {
		if rec["kind"] == "federation_sync_status" {
			found = rec["data"].(map[string]any)
			break
		}
	}
	require.NotNil(t, found, "expected federation_sync_status record")
	assert.Equal(t, float64(p.ID), found["project_id"])
	assert.Equal(t, "2026-05-23T01:00:00.000Z", found["last_pull_started_at"])
	assert.Equal(t, "2026-05-23T01:00:01.000Z", found["last_pull_success_at"])
	assert.Equal(t, "2026-05-23T01:00:02.000Z", found["last_push_started_at"])
	assert.Equal(t, "2026-05-23T01:00:03.000Z", found["last_error_at"])
	assert.Equal(t, assert.AnError.Error(), found["last_error"])
}

func indexOfKind(kinds []string, want string) int {
	for i, got := range kinds {
		if got == want {
			return i
		}
	}
	return -1
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

func TestExportUsesSingleSnapshot(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	w := &mutatingExportWriter{
		triggerNeedle: []byte(`"kind":"project"`),
		trigger: func() {
			createTesterIssue(ctx, t, d, p.ID, "created during export", "")
		},
	}

	require.NoError(t, jsonl.Export(ctx, d, w, jsonl.ExportOptions{IncludeDeleted: true}))

	records := decodeJSONLLines(t, w.Bytes())
	assertRecordsDoNotContain(t, records, "created during export")
}

type mutatingExportWriter struct {
	bytes.Buffer
	triggerNeedle []byte
	triggered     bool
	trigger       func()
}

func (w *mutatingExportWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if !w.triggered && bytes.Contains(p, w.triggerNeedle) {
		w.triggered = true
		w.trigger()
	}
	return n, err
}

func TestExportNoIncludeDeletedOmitsSoftDeletedIssueDependents(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	kept := createTesterIssue(ctx, t, d, p.ID, "kept issue", "")
	deleted := createTesterIssue(ctx, t, d, p.ID, "deleted issue", "", "gone")
	addTesterComment(ctx, t, d, deleted.ID, "deleted comment")
	_, _, err := d.CreateLinkAndEvent(ctx, db.CreateLinkParams{
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
	payload := json.RawMessage(bs)
	expectedHash, err := db.EventContentHash(db.EventHashInput{
		UID:               aggregated["uid"].(string),
		OriginInstanceUID: aggregated["origin_instance_uid"].(string),
		ProjectUID:        p.UID,
		ProjectName:       aggregated["project_name"].(string),
		IssueUID:          ptrToStringValue(t, aggregated["issue_uid"]),
		RelatedIssueUID:   ptrToStringValue(t, aggregated["related_issue_uid"]),
		Type:              aggregated["type"].(string),
		Actor:             aggregated["actor"].(string),
		HLCPhysicalMS:     int64(aggregated["hlc_physical_ms"].(float64)),
		HLCCounter:        int64(aggregated["hlc_counter"].(float64)),
		CreatedAt:         aggregated["created_at"].(string),
		Payload:           payload,
	})
	require.NoError(t, err)
	assert.Equal(t, expectedHash, aggregated["content_hash"],
		"live-only export must rehash scrubbed portable event fields")
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
		                     type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
		 VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', ?, ?,
		         ?, ?, 999, '01HZZZZZZZZZZZZZZZZZZZZA99',
		         'issue.linked', 'tester', '{}', 1, 0,
		         '0000000000000000000000000000000000000000000000000000000000000000')`,
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

func TestExportNoIncludeDeletedDropsFederatedEventForSoftDeletedIssueUID(t *testing.T) {
	ctx, d, p := newExportEnv(t)
	issue := createTesterIssue(ctx, t, d, p.ID, "federated subject", "")
	_, err := d.ExecContext(ctx, `UPDATE issues SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, issue.ID)
	require.NoError(t, err)
	const eventUID = "01HZZZZZZZZZZZZZZZZZRELOR1"
	_, err = d.ExecContext(ctx,
		`INSERT INTO events (uid, origin_instance_uid, project_id, project_name,
		                     issue_id, issue_uid, related_issue_id, related_issue_uid,
		                     type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
		 VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', ?, ?,
		         NULL, ?, NULL, NULL,
		         'issue.updated', 'remote', '{}', 1, 0,
		         '1111111111111111111111111111111111111111111111111111111111111111')`,
		eventUID, p.ID, p.Name, issue.UID)
	require.NoError(t, err)

	records := exportAndDecode(ctx, t, d, jsonl.ExportOptions{IncludeDeleted: false})

	for _, rec := range records {
		if rec["kind"] != "event" {
			continue
		}
		data, _ := rec["data"].(map[string]any)
		assert.NotEqual(t, eventUID, data["uid"], "live-only export must drop events attached only by deleted issue_uid")
	}
}

func openExportTestDB(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func writeLegacyV12FederationDB(t *testing.T, path string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	current, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := current.CreateProject(ctx, "legacy-fed")
	require.NoError(t, err)
	_, err = current.ExecContext(ctx, `
		INSERT INTO federation_bindings(
			project_id, role, hub_url, hub_project_id, hub_project_uid,
			replay_horizon_event_id, pull_cursor_event_id, push_enabled,
			push_cursor_event_id, bound_actor, enabled
		)
		VALUES(?, 'spoke', 'http://hub:7373', 42, ?, 7, 6, 1, 5, 'legacy-actor', 1)`,
		project.ID, project.UID)
	require.NoError(t, err)
	_, err = current.ExecContext(ctx, `
		INSERT INTO federation_enrollments(token_hash, spoke_instance_uid, project_id, capabilities, bound_actor)
		VALUES(?, ?, ?, 'pull,push', 'legacy-actor')`,
		strings.Repeat("a", 64), project.UID, project.ID)
	require.NoError(t, err)
	require.NoError(t, current.Close())

	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	_, err = raw.Exec(`
		PRAGMA foreign_keys = OFF;

		DROP INDEX IF EXISTS idx_federation_bindings_role_enabled;
		ALTER TABLE federation_bindings RENAME TO federation_bindings_current;
		CREATE TABLE federation_bindings (
		  project_id              INTEGER PRIMARY KEY REFERENCES projects(id),
		  role                    TEXT NOT NULL CHECK(role IN ('hub','spoke')),
		  hub_url                 TEXT NOT NULL DEFAULT '',
		  hub_project_id          INTEGER NOT NULL DEFAULT 0,
		  hub_project_uid         TEXT NOT NULL,
		  replay_horizon_event_id INTEGER NOT NULL DEFAULT 0,
		  pull_cursor_event_id    INTEGER NOT NULL DEFAULT 0,
		  push_enabled            INTEGER NOT NULL DEFAULT 0 CHECK(push_enabled IN (0,1)),
		  push_cursor_event_id    INTEGER NOT NULL DEFAULT 0 CHECK(push_cursor_event_id >= 0),
		  enabled                 INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
		  created_at              DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  updated_at              DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  last_sync_at            DATETIME,
		  CHECK (length(hub_project_uid) = 26),
		  CHECK (role = 'hub' OR length(trim(hub_url)) > 0),
		  CHECK (role = 'hub' OR hub_project_id > 0),
		  CHECK (replay_horizon_event_id >= 0),
		  CHECK (pull_cursor_event_id >= 0)
		);
		INSERT INTO federation_bindings(
		  project_id, role, hub_url, hub_project_id, hub_project_uid,
		  replay_horizon_event_id, pull_cursor_event_id, push_enabled,
		  push_cursor_event_id, enabled, created_at, updated_at, last_sync_at
		)
		SELECT project_id, role, hub_url, hub_project_id, hub_project_uid,
		       replay_horizon_event_id, pull_cursor_event_id, push_enabled,
		       push_cursor_event_id, enabled, created_at, updated_at, last_sync_at
		  FROM federation_bindings_current;
		DROP TABLE federation_bindings_current;
		CREATE INDEX idx_federation_bindings_role_enabled
		  ON federation_bindings(role, enabled);

		DROP INDEX IF EXISTS idx_federation_enrollments_scope;
		DROP INDEX IF EXISTS idx_federation_enrollments_spoke;
		ALTER TABLE federation_enrollments RENAME TO federation_enrollments_current;
		CREATE TABLE federation_enrollments (
		  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		  token_hash          TEXT NOT NULL UNIQUE,
		  spoke_instance_uid  TEXT NOT NULL,
		  project_id          INTEGER REFERENCES projects(id),
		  capabilities        TEXT NOT NULL,
		  created_at          DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  updated_at          DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  revoked_at          DATETIME,
		  CHECK (length(token_hash) = 64),
		  CHECK (length(spoke_instance_uid) = 26),
		  CHECK (length(trim(capabilities)) > 0)
		);
		INSERT INTO federation_enrollments(
		  id, token_hash, spoke_instance_uid, project_id, capabilities,
		  created_at, updated_at, revoked_at
		)
		SELECT id, token_hash, spoke_instance_uid, project_id, capabilities,
		       created_at, updated_at, revoked_at
		  FROM federation_enrollments_current;
		DROP TABLE federation_enrollments_current;
		CREATE INDEX idx_federation_enrollments_scope
		  ON federation_enrollments(project_id, revoked_at);
		CREATE INDEX idx_federation_enrollments_spoke
		  ON federation_enrollments(spoke_instance_uid);

		UPDATE meta SET value = '12' WHERE key = 'schema_version'`)
	require.NoError(t, err)
}

func writeLegacyV13FederationDB(t *testing.T, path string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	current, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := current.CreateProject(ctx, "legacy-fed")
	require.NoError(t, err)
	_, err = current.ExecContext(ctx, `
		INSERT INTO federation_bindings(
			project_id, role, hub_url, hub_project_id, hub_project_uid,
			replay_horizon_event_id, pull_cursor_event_id, push_enabled,
			push_cursor_event_id, bound_actor, enabled
		)
		VALUES(?, 'spoke', 'http://hub:7373', 42, ?, 7, 6, 1, 5, 'legacy-actor', 1)`,
		project.ID, project.UID)
	require.NoError(t, err)
	_, err = current.ExecContext(ctx, `
		INSERT INTO federation_enrollments(token_hash, spoke_instance_uid, project_id, capabilities, bound_actor)
		VALUES(?, ?, ?, 'pull,push', 'legacy-actor')`,
		strings.Repeat("b", 64), project.UID, project.ID)
	require.NoError(t, err)
	_, err = current.ExecContext(ctx, `UPDATE meta SET value = '13' WHERE key = 'schema_version'`)
	require.NoError(t, err)
	require.NoError(t, current.Close())
}

// writeLegacyV17GitHubStatusDB builds a schema_version=17 database with the
// pre-neutral-storage github_sync_* tables. The version-aware exporter must
// read those legacy tables and emit current issue_sync_* records.
func writeLegacyV17GitHubStatusDB(t *testing.T, path string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	current, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := current.CreateProject(ctx, "legacy-github")
	require.NoError(t, err)
	binding, err := current.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID:       project.ID,
		Provider:        "github",
		SourceKey:       "github:legacy-repo-node",
		RemoteID:        "legacy-repo-node",
		DisplayName:     "example-org/legacy-repo",
		Config:          []byte(`{"host":"github.com","owner":"example-org","repo":"legacy-repo","repo_id":7}`),
		IntervalSeconds: 900,
	})
	require.NoError(t, err)
	require.NoError(t, current.Close())

	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	_, err = raw.Exec(`
		PRAGMA foreign_keys = OFF;

		CREATE TABLE github_sync_bindings (
		  id                      INTEGER PRIMARY KEY AUTOINCREMENT,
		  project_id              INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		  source_key              TEXT NOT NULL,
		  host                    TEXT NOT NULL,
		  owner                   TEXT NOT NULL,
		  repo                    TEXT NOT NULL,
		  repo_node_id            TEXT NOT NULL,
		  repo_id                 INTEGER NOT NULL,
		  enabled                 INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
		  interval_seconds        INTEGER NOT NULL,
		  last_cursor_at          DATETIME,
		  created_at              DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  updated_at              DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
		INSERT INTO github_sync_bindings(
		  id, project_id, source_key, host, owner, repo, repo_node_id, repo_id,
		  enabled, interval_seconds, last_cursor_at, created_at, updated_at
		)
		VALUES(
		  ?, ?, 'github:legacy-repo-node', 'github.com', 'example-org',
		  'legacy-repo', 'legacy-repo-node', 7, 1, 900, NULL,
		  '2026-06-01T09:00:00.000Z', '2026-06-01T10:01:00.000Z'
		);

		CREATE TABLE github_sync_status (
		  binding_id              INTEGER PRIMARY KEY REFERENCES github_sync_bindings(id) ON DELETE CASCADE,
		  project_id              INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		  sync_started_at         DATETIME,
		  last_attempt_at         DATETIME,
		  last_success_at         DATETIME,
		  last_error_at           DATETIME,
		  last_error              TEXT,
		  last_issue_count        INTEGER NOT NULL DEFAULT 0,
		  last_comment_count      INTEGER NOT NULL DEFAULT 0,
		  last_import_event_count INTEGER NOT NULL DEFAULT 0,
		  CHECK (last_issue_count >= 0),
		  CHECK (last_comment_count >= 0),
		  CHECK (last_import_event_count >= 0)
		);
		INSERT INTO github_sync_status(
		  binding_id, project_id, sync_started_at, last_attempt_at,
		  last_success_at, last_error_at, last_error,
		  last_issue_count, last_comment_count, last_import_event_count
		)
		VALUES(
		  ?, ?, NULL, '2026-06-01T09:58:00.000Z',
		  '2026-06-01T10:00:00.000Z', '2026-06-01T10:02:00.000Z',
		  'rate limited', 9, 5, 14
		);

		UPDATE meta SET value = '17' WHERE key = 'schema_version'`, binding.ID, project.ID, binding.ID, project.ID)
	require.NoError(t, err)
	require.Positive(t, binding.ID)
}

// newExportEnv opens a fresh test DB and seeds the canonical "kata" project
// used by most export tests.
func newExportEnv(t *testing.T) (context.Context, *sqlitestore.Store, db.Project) {
	t.Helper()
	ctx := context.Background()
	d := openExportTestDB(t)
	p, err := d.CreateProject(ctx, "kata")
	require.NoError(t, err)
	return ctx, d, p
}

// exportAndDecode runs jsonl.Export into a buffer and decodes the resulting
// JSONL stream into records.
func exportAndDecode(ctx context.Context, t *testing.T, d *sqlitestore.Store, opts jsonl.ExportOptions) []map[string]any {
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

func assertRecordsContain(t *testing.T, records []map[string]any, needle string) {
	t.Helper()
	for _, rec := range records {
		bs, err := json.Marshal(rec)
		require.NoError(t, err)
		if strings.Contains(string(bs), needle) {
			return
		}
	}
	t.Fatalf("expected exported records to contain %q", needle)
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

func ptrToStringValue(t *testing.T, v any) *string {
	t.Helper()
	if v == nil {
		return nil
	}
	s, ok := v.(string)
	require.True(t, ok, "expected string value, got %T", v)
	return &s
}

func firstKindIndex(records []map[string]any, kind string) int {
	for i, rec := range records {
		if rec["kind"] == kind {
			return i
		}
	}
	return -1
}

func assertKindOrder(t *testing.T, records []map[string]any) {
	t.Helper()
	order := map[string]int{
		"meta": 0, "project": 1, "project_alias": 2, "issue_sync_binding": 3,
		"issue_sync_status": 4, "recurrence": 5, "issue": 6, "comment": 7,
		"issue_label": 8, "link": 9, "import_mapping": 10, "federation_binding": 11,
		"federation_sync_status": 12, "federation_quarantine": 13, "federation_enrollment": 14,
		"issue_claim": 15, "pending_claim_request": 16, "event": 17, "purge_log": 18,
		"project_purge_log": 19, "sqlite_sequence": 20,
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
