package jsonl_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/uid"
)

func TestRoundtripRichDatabaseIsByteEquivalent(t *testing.T) {
	ctx := context.Background()
	fixture := buildRichJSONLFixture(t)
	src := fixture.DB

	first := exportToBuffer(ctx, t, src)

	dst := openExportTestDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(first.Bytes()), dst))

	second := exportToBuffer(ctx, t, dst)

	assert.Equal(t, first.String(), second.String())
	assertRoundtripTableCounts(t, src, dst)
	assertSQLiteSequenceRows(t, src, dst)
	assertSearchResultsMatch(t, src, dst, fixture.Project.ID, "orchid")
	assertSearchResultsMatch(t, src, dst, fixture.Project.ID, "watermelon")
	assertSearchResultsMatch(t, src, dst, fixture.Project.ID, "soft")
}

func TestRoundtrip_ReplaysAPITokensFromTokenEvents(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	name := "laptop"
	active, _, err := src.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "active-token",
		Actor:          "wesm",
		Name:           &name,
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	revoked, _, err := src.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "revoked-token",
		Actor:          "ci",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = src.RevokeAPIToken(ctx, revoked.ID, db.BootstrapActor)
	require.NoError(t, err)
	_, err = src.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		active.ID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{}))

	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dst))

	tokens, err := dst.ListAPITokens(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 2)
	assert.Empty(t, tokens[0].TokenHash)
	assert.Nil(t, tokens[0].LastUsedAt, "last_used_at is projection-only and resets on import")
	assert.NotNil(t, tokens[1].RevokedAt)

	got, err := dst.ResolveAPIToken(ctx, "active-token")
	require.NoError(t, err)
	assert.Equal(t, active.ID, got.ID)
	assert.Equal(t, "wesm", got.Actor)
	require.NotNil(t, got.Name)
	assert.Equal(t, "laptop", *got.Name)

	_, err = dst.ResolveAPIToken(ctx, "revoked-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestRoundtrip_DuplicateTokenRevokedEventsKeepFirstRevokedAt(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	tok, _, err := src.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "revoked-token",
		Actor:          "ci",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, firstRevokedEvent, err := src.RevokeAPIToken(ctx, tok.ID, db.BootstrapActor)
	require.NoError(t, err)
	var firstRevokedAt string
	require.NoError(t, src.QueryRowContext(ctx,
		`SELECT CAST(created_at AS TEXT) FROM events WHERE id = ?`, firstRevokedEvent.ID).Scan(&firstRevokedAt))

	system, err := src.SystemProject(ctx)
	require.NoError(t, err)
	eventUID, err := uid.New()
	require.NoError(t, err)
	originUID := src.InstanceUID()
	payload := fmt.Sprintf(`{"token_id":%d}`, tok.ID)
	createdAt := "2026-05-28T23:59:59.000Z"
	hlcPhysicalMS := int64(1780012799000)
	hlcCounter := int64(99)
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               eventUID,
		OriginInstanceUID: originUID,
		ProjectUID:        system.UID,
		ProjectName:       system.Name,
		Type:              "token.revoked",
		Actor:             db.BootstrapActor,
		HLCPhysicalMS:     hlcPhysicalMS,
		HLCCounter:        hlcCounter,
		CreatedAt:         createdAt,
		Payload:           json.RawMessage(payload),
	})
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, `
		INSERT INTO events(uid, origin_instance_uid, project_id, project_name, type, actor, payload,
		                   hlc_physical_ms, hlc_counter, content_hash, created_at)
		VALUES(?, ?, ?, ?, 'token.revoked', ?, ?, ?, ?, ?, ?)`,
		eventUID, originUID, system.ID, system.Name, db.BootstrapActor,
		payload, hlcPhysicalMS, hlcCounter, hash, createdAt)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{}))
	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dst))

	var got string
	require.NoError(t, dst.QueryRowContext(ctx,
		`SELECT CAST(revoked_at AS TEXT) FROM api_tokens WHERE id = ?`, tok.ID).Scan(&got))
	assert.Equal(t, firstRevokedAt, got)
}

func assertRoundtripTableCounts(t *testing.T, src, dst *sqlitestore.Store) {
	t.Helper()
	for _, table := range []string{
		"projects", "project_aliases", "issues", "comments", "issue_labels",
		"links", "import_mappings", "federation_bindings", "federation_enrollments",
		"federation_sync_status", "federation_quarantine", "issue_claims", "pending_claim_requests", "events", "purge_log",
	} {
		assertTableCount(t, src, dst, table)
	}
}

func TestFederationSyncStatusRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcDB := openExportTestDB(t)
	p, err := srcDB.CreateProject(ctx, "sync-status")
	require.NoError(t, err)
	require.NoError(t, srcDB.RecordFederationSyncPullStarted(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:00.000Z")))
	require.NoError(t, srcDB.RecordFederationSyncPullSuccess(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:01.000Z")))
	require.NoError(t, srcDB.RecordFederationSyncPushStarted(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:02.000Z")))
	require.NoError(t, srcDB.RecordFederationSyncPushSuccess(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:03.000Z")))
	require.NoError(t, srcDB.RecordFederationSyncReset(ctx, p.ID, mustParseTime(t, "2026-05-23T01:00:04.000Z")))
	require.NoError(t, srcDB.RecordFederationSyncError(ctx, p.ID, fmt.Errorf("poll failed"), mustParseTime(t, "2026-05-23T01:00:05.000Z")))

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dstDB))

	got, err := dstDB.FederationSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ProjectID)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:00.000Z"), got.LastPullStartedAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:01.000Z"), got.LastPullSuccessAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:02.000Z"), got.LastPushStartedAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:03.000Z"), got.LastPushSuccessAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:04.000Z"), got.LastResetAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:05.000Z"), got.LastErrorAt)
	require.NotNil(t, got.LastError)
	assert.Equal(t, "poll failed", *got.LastError)
}

func TestFederationQuarantineRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcDB := openExportTestDB(t)
	p, err := srcDB.CreateProject(ctx, "quarantine")
	require.NoError(t, err)
	_, err = srcDB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Enabled:              true,
	})
	require.NoError(t, err)
	created, err := srcDB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    p.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7", "evt-8"},
		Error:        "poison",
		CreatedAt:    mustParseTime(t, "2026-05-23T01:00:00.000Z"),
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))
	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dstDB))

	got, err := dstDB.ActiveFederationQuarantine(ctx, p.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, []string{"evt-7", "evt-8"}, got.EventUIDs)
	assert.Equal(t, "poison", got.Error)
}

func TestClaimRoundTripRows(t *testing.T) {
	ctx := context.Background()
	srcDB := openExportTestDB(t)
	p, err := srcDB.CreateProject(ctx, "claims")
	require.NoError(t, err)
	issue, _, err := srcDB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "claimed",
		Author:    "tester",
	})
	require.NoError(t, err)

	claimUID := "01J00000000000000000000001"
	holderInstanceUID := "01J00000000000000000000002"
	requestUID := "01J00000000000000000000003"
	_, err = srcDB.ExecContext(ctx, `
		INSERT INTO issue_claims(
		  claim_uid, project_id, issue_id, issue_uid, holder,
		  holder_instance_uid, client_kind, purpose, claim_kind,
		  acquired_at, revision, updated_at
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'hard', ?, 2, ?)`,
		claimUID, p.ID, issue.ID, issue.UID, "tester",
		holderInstanceUID, "cli", "editing",
		"2026-05-23T12:00:00.000Z", "2026-05-23T12:30:00.000Z")
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx, `
		INSERT INTO pending_claim_requests(
		  request_uid, project_id, issue_id, issue_uid, holder,
		  holder_instance_uid, client_kind, claim_kind, ttl_seconds, purpose, requested_at,
		  last_attempt_at, last_error
		)
		VALUES(?, ?, ?, ?, ?, ?, 'cli', 'timed', 900, ?, ?, ?, ?)`,
		requestUID, p.ID, issue.ID, issue.UID, "tester",
		holderInstanceUID, "offline edit", "2026-05-23T12:01:00.000Z",
		"2026-05-23T12:02:00.000Z", "hub offline")
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dstDB))

	var gotClaimUID, gotIssueUID, gotHolderInstanceUID string
	var gotReleasedAt sql.NullString
	var gotRevision int64
	require.NoError(t, dstDB.QueryRowContext(ctx, `
		SELECT claim_uid, issue_uid, holder_instance_uid, CAST(released_at AS TEXT), revision
		  FROM issue_claims
		 WHERE claim_uid = ?`, claimUID).Scan(
		&gotClaimUID, &gotIssueUID, &gotHolderInstanceUID, &gotReleasedAt, &gotRevision))
	assert.Equal(t, claimUID, gotClaimUID)
	assert.Equal(t, issue.UID, gotIssueUID)
	assert.Equal(t, holderInstanceUID, gotHolderInstanceUID)
	assert.False(t, gotReleasedAt.Valid, "active claim must stay unreleased")
	assert.Equal(t, int64(2), gotRevision)

	var gotRequestUID, gotPendingIssueUID, gotPendingHolderInstanceUID, gotPendingClientKind string
	var gotRejectedAt, gotResolvedAt sql.NullString
	require.NoError(t, dstDB.QueryRowContext(ctx, `
		SELECT request_uid, issue_uid, holder_instance_uid, client_kind, CAST(rejected_at AS TEXT), CAST(resolved_at AS TEXT)
		  FROM pending_claim_requests
		 WHERE request_uid = ?`, requestUID).Scan(
		&gotRequestUID, &gotPendingIssueUID, &gotPendingHolderInstanceUID,
		&gotPendingClientKind, &gotRejectedAt, &gotResolvedAt))
	assert.Equal(t, requestUID, gotRequestUID)
	assert.Equal(t, issue.UID, gotPendingIssueUID)
	assert.Equal(t, holderInstanceUID, gotPendingHolderInstanceUID)
	assert.Equal(t, "cli", gotPendingClientKind)
	assert.False(t, gotRejectedAt.Valid, "pending request must stay unrejected")
	assert.False(t, gotResolvedAt.Valid, "pending request must stay unresolved")
}

func assertSQLiteSequenceRows(t *testing.T, src, dst *sqlitestore.Store) {
	t.Helper()
	assert.Equal(t, sqliteSequenceSnapshot(t, src), sqliteSequenceSnapshot(t, dst))
}

func sqliteSequenceSnapshot(t *testing.T, d *sqlitestore.Store) []map[string]any {
	t.Helper()
	rows, err := d.Query(`SELECT name, seq FROM sqlite_sequence ORDER BY name ASC`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var name string
		var seq int64
		require.NoError(t, rows.Scan(&name, &seq))
		out = append(out, map[string]any{"name": name, "seq": seq})
	}
	require.NoError(t, rows.Err())
	return out
}

func assertSearchResultsMatch(t *testing.T, src, dst *sqlitestore.Store, projectID int64, query string) {
	t.Helper()
	srcHits, err := src.SearchFTS(context.Background(), projectID, query, 20, true)
	require.NoError(t, err)
	dstHits, err := dst.SearchFTS(context.Background(), projectID, query, 20, true)
	require.NoError(t, err)
	assert.Equal(t, normalizeSearchHits(srcHits), normalizeSearchHits(dstHits), query)
}

func normalizeSearchHits(hits []db.SearchCandidate) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		out = append(out, map[string]any{
			"project_id": hit.Issue.ProjectID,
			"short_id":   hit.Issue.ShortID,
			"title":      hit.Issue.Title,
			"matched_in": hit.MatchedIn,
			"deleted":    hit.Issue.DeletedAt != nil,
		})
	}
	return out
}

// TestRoundtrip_PurgeLogEnvelopeCarriesShortID pins the purge-tombstone
// extension: a purge_log row's short_id snapshot must survive export and
// land on the destination so the tombstone keeps gating reuse after a
// cutover or backup restore.
func TestRoundtrip_PurgeLogEnvelopeCarriesShortID(t *testing.T) {
	ctx := context.Background()
	d := openExportTestDB(t)
	p, err := d.CreateProject(ctx, "demo")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "to be purged",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.NotEmpty(t, issue.ShortID)
	_, err = d.PurgeIssue(ctx, issue.ID, "tester", nil)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, d, &buf, jsonl.ExportOptions{}))
	exported := buf.Bytes()

	scanner := bufio.NewScanner(bytes.NewReader(exported))
	var purgePayload map[string]any
	for scanner.Scan() {
		var env jsonl.Envelope
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &env))
		if env.Kind == jsonl.KindPurgeLog {
			require.NoError(t, json.Unmarshal(env.Data, &purgePayload))
			break
		}
	}
	require.NotNil(t, purgePayload, "exported stream must include a purge_log envelope")
	assert.Equal(t, issue.ShortID, purgePayload["short_id"])

	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(exported), dst))

	var got *string
	require.NoError(t, dst.QueryRowContext(ctx,
		`SELECT short_id FROM purge_log WHERE project_id = ?`, p.ID).Scan(&got))
	require.NotNil(t, got)
	assert.Equal(t, issue.ShortID, *got)
}

func TestRoundtrip_FederationBindingCarriesPushState(t *testing.T) {
	ctx := context.Background()
	srcDB := openExportTestDB(t)
	p, err := srcDB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding := db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 7,
		PullCursorEventID:    6,
		PushEnabled:          true,
		PushCursorEventID:    5,
		Enabled:              true,
	}
	_, err = srcDB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))

	var bindingPayload map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for scanner.Scan() {
		var env jsonl.Envelope
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &env))
		if env.Kind == jsonl.KindFederationBinding {
			require.NoError(t, json.Unmarshal(env.Data, &bindingPayload))
			break
		}
	}
	require.NoError(t, scanner.Err())
	require.NotNil(t, bindingPayload)
	assert.Equal(t, true, bindingPayload["push_enabled"])
	assert.Equal(t, float64(5), bindingPayload["push_cursor_event_id"])
	assert.NotContains(t, bindingPayload, "materialize_after_event_id")

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dstDB))
	got, err := dstDB.FederationBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.True(t, got.PushEnabled)
	assert.Equal(t, int64(5), got.PushCursorEventID)
}

func TestRoundtrip_FederationEnrollmentRows(t *testing.T) {
	ctx := context.Background()
	srcDB := openExportTestDB(t)
	p, err := srcDB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	tokenHash := strings.Repeat("b", 64)
	plaintextToken := "plaintext-token-never-exported"
	_, err = srcDB.ExecContext(ctx, `
		INSERT INTO federation_enrollments(token_hash, spoke_instance_uid, project_id, capabilities)
		VALUES(?, ?, ?, ?)`,
		tokenHash, "01HZZZZZZZZZZZZZZZZZZZZZ02", p.ID, "pull,push")
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))
	assert.Contains(t, buf.String(), tokenHash)
	assert.NotContains(t, buf.String(), plaintextToken)

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dstDB))

	var gotHash, gotSpoke, gotCapabilities string
	var gotProjectID int64
	require.NoError(t, dstDB.QueryRowContext(ctx, `
		SELECT token_hash, spoke_instance_uid, project_id, capabilities
		  FROM federation_enrollments`).Scan(&gotHash, &gotSpoke, &gotProjectID, &gotCapabilities))
	assert.Equal(t, tokenHash, gotHash)
	assert.Equal(t, "01HZZZZZZZZZZZZZZZZZZZZZ02", gotSpoke)
	assert.Equal(t, p.ID, gotProjectID)
	assert.Equal(t, "pull,push", gotCapabilities)
}

// TestRoundtrip_IssueEnvelopeCarriesShortID pins spec §8.1: the JSONL issue
// envelope carries a short_id field at the current schema version and drops
// the legacy number field. The cutover (Task 9) handles older inputs.
func TestRoundtrip_IssueEnvelopeCarriesShortID(t *testing.T) {
	ctx := context.Background()
	d := openExportTestDB(t)
	p, err := d.CreateProject(ctx, "demo")
	require.NoError(t, err)
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "rt",
		Author:    "tester",
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, d, &buf, jsonl.ExportOptions{}))

	scanner := bufio.NewScanner(&buf)
	var issuePayload map[string]any
	for scanner.Scan() {
		var env jsonl.Envelope
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &env))
		if env.Kind == jsonl.KindIssue {
			require.NoError(t, json.Unmarshal(env.Data, &issuePayload))
			break
		}
	}
	require.NotNil(t, issuePayload)
	assert.Equal(t, created.ShortID, issuePayload["short_id"])
	_, hasNumber := issuePayload["number"]
	assert.False(t, hasNumber, "issue envelope should not carry 'number'")
}

func TestRoundtrip_IssuePreservesMetadataAndRevision(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, err := srcDB.CreateProject(ctx, "p")
	require.NoError(t, err)
	iss, _, err := srcDB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx,
		`UPDATE issues SET metadata = ?, revision = ? WHERE id = ?`,
		`{"scheduled_on":"2026-05-20","someday":false}`, int64(7), iss.ID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dstDB))

	var meta string
	var rev int64
	require.NoError(t, dstDB.QueryRowContext(ctx,
		`SELECT metadata, revision FROM issues LIMIT 1`).Scan(&meta, &rev))
	assert.JSONEq(t, `{"scheduled_on":"2026-05-20","someday":false}`, meta)
	assert.Equal(t, int64(7), rev)
}

func TestRoundtrip_ProjectPreservesMetadataAndRevision(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, err := srcDB.CreateProject(ctx, "Personal")
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx,
		`UPDATE projects SET metadata = ?, revision = ? WHERE id = ?`,
		`{"area":"Personal","sidebar_order":2}`, int64(4), p.ID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))
	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dstDB))

	var meta string
	var rev int64
	require.NoError(t, dstDB.QueryRowContext(ctx,
		`SELECT metadata, revision FROM projects WHERE name = 'Personal'`,
	).Scan(&meta, &rev))
	assert.JSONEq(t, `{"area":"Personal","sidebar_order":2}`, meta)
	assert.Equal(t, int64(4), rev)
}

func TestRoundtrip_RecurrenceFullFields(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, err := srcDB.CreateProject(ctx, "p")
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"REC00000000000000000000001", p.ID, "FREQ=WEEKLY;BYDAY=MO", "2026-05-11",
		"America/New_York", "Weekly review", "What got done?",
		`["chore","weekly"]`, `{}`, "tester")
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))
	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dstDB))

	var rule, dtstart, tz, title, labels string
	require.NoError(t, dstDB.QueryRowContext(ctx,
		`SELECT rrule, dtstart, timezone, template_title, template_labels
		   FROM recurrences LIMIT 1`,
	).Scan(&rule, &dtstart, &tz, &title, &labels))
	assert.Equal(t, "FREQ=WEEKLY;BYDAY=MO", rule)
	assert.Equal(t, "2026-05-11", dtstart)
	assert.Equal(t, "America/New_York", tz)
	assert.Equal(t, "Weekly review", title)
	assert.JSONEq(t, `["chore","weekly"]`, labels)
}

func TestRoundtrip_IssueRecurrenceLinkagePreserved(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, err := srcDB.CreateProject(ctx, "p")
	require.NoError(t, err)

	// Insert a recurrence directly.
	recUID := "REC00000000000000000000001"
	res, err := srcDB.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		recUID, p.ID, "FREQ=WEEKLY", "2026-05-11", "UTC", "Weekly", "",
		`[]`, `{}`, "tester")
	require.NoError(t, err)
	recID, err := res.LastInsertId()
	require.NoError(t, err)

	// Create an issue that references this recurrence via raw UPDATE.
	iss, _, err := srcDB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "Weekly", Author: "tester",
	})
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx,
		`UPDATE issues SET recurrence_id = ?, occurrence_key = ? WHERE id = ?`,
		recID, "2026-05-11", iss.ID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))
	require.Contains(t, buf.String(), `"recurrence_uid":"`+recUID+`"`)

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dstDB))

	var gotRecID int64
	var gotOccKey string
	require.NoError(t, dstDB.QueryRowContext(ctx,
		`SELECT recurrence_id, occurrence_key FROM issues WHERE id = ?`, iss.ID,
	).Scan(&gotRecID, &gotOccKey))
	assert.Equal(t, recID, gotRecID)
	assert.Equal(t, "2026-05-11", gotOccKey)
}

func TestImport_RecurrenceUIDAndIDDisagree_Errors(t *testing.T) {
	dstDB := openImportTargetDB(t)
	body := bytes.NewBufferString("")
	body.WriteString(`{"kind":"meta","data":{"key":"export_version","value":"10"}}` + "\n")
	body.WriteString(`{"kind":"meta","data":{"key":"instance_uid","value":"ORIGIN0000000000000000000A"}}` + "\n")
	body.WriteString(`{"kind":"project","data":{"id":1,"uid":"PROJ000000000000000000000A","name":"p","metadata":{},"revision":1,"created_at":"2026-05-15T00:00:00.000Z"}}` + "\n")
	body.WriteString(`{"kind":"recurrence","data":{"id":1,"uid":"RECA000000000000000000000A","project_id":1,"rrule":"FREQ=WEEKLY","dtstart":"2026-05-15","timezone":"UTC","template_title":"a","template_body":"","template_labels":[],"template_metadata":{},"author":"t","revision":1,"created_at":"2026-05-15T00:00:00.000Z","updated_at":"2026-05-15T00:00:00.000Z"}}` + "\n")
	body.WriteString(`{"kind":"recurrence","data":{"id":2,"uid":"RECB000000000000000000000A","project_id":1,"rrule":"FREQ=MONTHLY","dtstart":"2026-05-01","timezone":"UTC","template_title":"b","template_body":"","template_labels":[],"template_metadata":{},"author":"t","revision":1,"created_at":"2026-05-15T00:00:00.000Z","updated_at":"2026-05-15T00:00:00.000Z"}}` + "\n")
	// Issue claims recurrence_uid=RECA but recurrence_id=2 (which is RECB).
	body.WriteString(`{"kind":"issue","data":{"id":1,"uid":"ISS00000000000000000000A00","project_id":1,"short_id":"0a00","title":"t","body":"","status":"open","author":"t","metadata":{},"revision":1,"recurrence_id":2,"recurrence_uid":"RECA000000000000000000000A","occurrence_key":"2026-05-15","created_at":"2026-05-15T00:00:00.000Z","updated_at":"2026-05-15T00:00:00.000Z"}}` + "\n")

	err := jsonl.Import(context.Background(), body, dstDB)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recurrence_uid")
}

func TestImport_OccurrenceKeyWithoutRecurrence_Errors(t *testing.T) {
	dstDB := openImportTargetDB(t)
	body := bytes.NewBufferString("")
	body.WriteString(`{"kind":"meta","data":{"key":"export_version","value":"10"}}` + "\n")
	body.WriteString(`{"kind":"meta","data":{"key":"instance_uid","value":"ORIGIN0000000000000000000A"}}` + "\n")
	body.WriteString(`{"kind":"project","data":{"id":1,"uid":"PROJ000000000000000000000A","name":"p","metadata":{},"revision":1,"created_at":"2026-05-15T00:00:00.000Z"}}` + "\n")
	body.WriteString(`{"kind":"issue","data":{"id":1,"uid":"ISS00000000000000000000A00","project_id":1,"short_id":"0a00","title":"t","body":"","status":"open","author":"t","metadata":{},"revision":1,"occurrence_key":"2026-05-15","created_at":"2026-05-15T00:00:00.000Z","updated_at":"2026-05-15T00:00:00.000Z"}}` + "\n")

	err := jsonl.Import(context.Background(), body, dstDB)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "occurrence_key")
}

func TestExport_RecurrencesAppearBeforeIssuesInStream(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, err := srcDB.CreateProject(ctx, "p")
	require.NoError(t, err)
	_, _, err = srcDB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, author)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"REC00000000000000000000002", p.ID, "FREQ=WEEKLY", "2026-05-11",
		"UTC", "x", "tester")
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))

	lines := strings.Split(buf.String(), "\n")
	firstRecurrence, firstIssue := -1, -1
	for i, l := range lines {
		if strings.Contains(l, `"kind":"recurrence"`) && firstRecurrence == -1 {
			firstRecurrence = i
		}
		if strings.Contains(l, `"kind":"issue"`) && firstIssue == -1 {
			firstIssue = i
		}
	}
	require.NotEqual(t, -1, firstRecurrence)
	require.NotEqual(t, -1, firstIssue)
	assert.Less(t, firstRecurrence, firstIssue,
		"recurrence records must appear before issue records — issues reference recurrences via FK")
}

func TestExport_SoftDeletedRecurrenceReferencedByIssueIsIncluded(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, _ := srcDB.CreateProject(ctx, "p")

	recUID := "REC00000000000000000000005"
	res, err := srcDB.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		recUID, p.ID, "FREQ=WEEKLY", "2026-05-11", "UTC", "x", "",
		`[]`, `{}`, "tester")
	require.NoError(t, err)
	recID, _ := res.LastInsertId()

	// Live issue referencing the recurrence.
	iss, _, err := srcDB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "instance", Author: "tester",
	})
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx,
		`UPDATE issues SET recurrence_id = ?, occurrence_key = ? WHERE id = ?`,
		recID, "2026-05-11", iss.ID)
	require.NoError(t, err)

	// Soft-delete the recurrence directly.
	_, err = srcDB.ExecContext(ctx,
		`UPDATE recurrences SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		recID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf,
		jsonl.ExportOptions{IncludeDeleted: false}))
	assert.Contains(t, buf.String(), `"kind":"recurrence"`,
		"soft-deleted recurrence referenced by a live issue must be included")

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dstDB))
	var n int
	require.NoError(t, dstDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues WHERE recurrence_id IS NOT NULL`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestExport_SoftDeletedRecurrenceWithNoLiveIssues_Excluded(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, _ := srcDB.CreateProject(ctx, "p")

	recUID := "REC00000000000000000000006"
	res, err := srcDB.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		recUID, p.ID, "FREQ=WEEKLY", "2026-05-11", "UTC", "x", "",
		`[]`, `{}`, "tester")
	require.NoError(t, err)
	recID, _ := res.LastInsertId()

	_, err = srcDB.ExecContext(ctx,
		`UPDATE recurrences SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		recID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf,
		jsonl.ExportOptions{IncludeDeleted: false}))
	assert.NotContains(t, buf.String(), `"kind":"recurrence"`,
		"soft-deleted recurrence with no live referrer must be excluded")
}

// TestRoundtrip_NewEventPayloadBytesAreExact locks in that the JSONL pipeline
// preserves event payload bytes verbatim — key order, whitespace, the lot.
// New event types (issue.metadata_updated, issue.moved, recurrence.materialized)
// rely on this so consumers reading raw payloads see exactly what the producer
// wrote. The guarantee depends on the export-side eventRecord using
// json.RawMessage (not interface{} / map[string]any, which would re-marshal
// and reorder keys).
// TestRoundtrip_MultipleRecurrencesAndInstances locks in the multi-row
// UID→ID resolution path: two recurrences (Weekly + Monthly) with five
// instances spread across them must each land with the correct recurrence_id
// FK and occurrence_key after a JSONL roundtrip.
func TestRoundtrip_MultipleRecurrencesAndInstances(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, err := srcDB.CreateProject(ctx, "p")
	require.NoError(t, err)

	// Insert two recurrences with distinct UIDs and template titles.
	weeklyUID := "RECW00000000000000000000A1"
	monthlyUID := "RECM00000000000000000000A1"

	resW, err := srcDB.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		weeklyUID, p.ID, "FREQ=WEEKLY", "2026-05-11", "UTC", "Weekly", "",
		`[]`, `{}`, "tester")
	require.NoError(t, err)
	weeklyID, err := resW.LastInsertId()
	require.NoError(t, err)

	resM, err := srcDB.ExecContext(ctx, `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		monthlyUID, p.ID, "FREQ=MONTHLY", "2026-05-01", "UTC", "Monthly", "",
		`[]`, `{}`, "tester")
	require.NoError(t, err)
	monthlyID, err := resM.LastInsertId()
	require.NoError(t, err)

	// Weekly instances: 2026-05-11, 2026-05-18, 2026-05-25.
	weeklyKeys := []string{"2026-05-11", "2026-05-18", "2026-05-25"}
	for _, key := range weeklyKeys {
		iss, _, err := srcDB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID, Title: "Weekly " + key, Author: "tester",
		})
		require.NoError(t, err)
		_, err = srcDB.ExecContext(ctx,
			`UPDATE issues SET recurrence_id = ?, occurrence_key = ? WHERE id = ?`,
			weeklyID, key, iss.ID)
		require.NoError(t, err)
	}

	// Monthly instances: 2026-05-01, 2026-06-01.
	monthlyKeys := []string{"2026-05-01", "2026-06-01"}
	for _, key := range monthlyKeys {
		iss, _, err := srcDB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID, Title: "Monthly " + key, Author: "tester",
		})
		require.NoError(t, err)
		_, err = srcDB.ExecContext(ctx,
			`UPDATE issues SET recurrence_id = ?, occurrence_key = ? WHERE id = ?`,
			monthlyID, key, iss.ID)
		require.NoError(t, err)
	}

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dstDB))

	// Assert 3 issues link to the Weekly recurrence.
	var weeklyCount int
	require.NoError(t, dstDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM issues i
		JOIN recurrences r ON r.id = i.recurrence_id
		WHERE r.template_title = 'Weekly'`).Scan(&weeklyCount))
	assert.Equal(t, 3, weeklyCount)

	// Assert 2 issues link to the Monthly recurrence.
	var monthlyCount int
	require.NoError(t, dstDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM issues i
		JOIN recurrences r ON r.id = i.recurrence_id
		WHERE r.template_title = 'Monthly'`).Scan(&monthlyCount))
	assert.Equal(t, 2, monthlyCount)

	// Assert occurrence_keys land under the *correct* recurrence — not just
	// that the right counts and key set survive globally. A bug that swapped
	// one weekly key with one monthly key while preserving cardinalities
	// would pass the counts above but fail this per-recurrence check.
	rows, err := dstDB.QueryContext(ctx, `
		SELECT r.template_title, i.occurrence_key
		  FROM issues i JOIN recurrences r ON r.id = i.recurrence_id
		 WHERE i.occurrence_key IS NOT NULL
		 ORDER BY r.template_title ASC, i.occurrence_key ASC`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	keysByRecurrence := map[string][]string{}
	for rows.Next() {
		var title, key string
		require.NoError(t, rows.Scan(&title, &key))
		keysByRecurrence[title] = append(keysByRecurrence[title], key)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string][]string{
		"Monthly": {"2026-05-01", "2026-06-01"},
		"Weekly":  {"2026-05-11", "2026-05-18", "2026-05-25"},
	}, keysByRecurrence)
}

func TestRoundtrip_NewEventPayloadBytesAreExact(t *testing.T) {
	srcDB := openExportTestDB(t)
	ctx := context.Background()
	p, err := srcDB.CreateProject(ctx, "p")
	require.NoError(t, err)
	iss, _, err := srcDB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)

	// Inject events with specific payloads. We're testing the JSONL layer, not
	// the producer code paths (§C / §F), so raw SQL is fine — the schema check
	// is json_valid only and the importer passes payload bytes through.
	cases := []struct {
		eventType string
		payload   string
	}{
		{
			"issue.metadata_updated",
			`{"diff":{"scheduled_on":{"from":null,"to":"2026-05-20"},"someday":{"from":null,"to":true}},"revision_new":2}`,
		},
		{
			"issue.moved",
			`{"from_project_id":1,"to_project_id":2,"new_short_id":"abc"}`,
		},
		{
			"recurrence.materialized",
			`{"recurrence_uid":"REC00000000000000000000001","occurrence_key":"2026-05-18","new_issue_uid":"ISS00000000000000000000A00"}`,
		},
	}
	for i, c := range cases {
		eventUID := fmt.Sprintf("EVT0000000000000000000%03d0", i+1)
		require.Equal(t, 26, len(eventUID), "test fixture event UID must be 26 chars")
		_, err := srcDB.ExecContext(ctx, `
			INSERT INTO events
			  (uid, origin_instance_uid, project_id, project_name,
			   issue_id, issue_uid, type, actor, payload,
			   hlc_physical_ms, hlc_counter, content_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0,
			        '0000000000000000000000000000000000000000000000000000000000000000')`,
			eventUID, srcDB.InstanceUID(), p.ID, "p",
			iss.ID, iss.UID, c.eventType, "tester", c.payload, i+1)
		require.NoError(t, err)
	}

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, srcDB, &buf, jsonl.ExportOptions{}))

	dstDB := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dstDB))

	// Byte-exact equality (not JSONEq) — key reordering would cause a
	// mismatch, which is the regression we're guarding against.
	for _, c := range cases {
		var gotPayload string
		require.NoError(t, dstDB.QueryRowContext(ctx,
			`SELECT payload FROM events WHERE type = ? LIMIT 1`, c.eventType,
		).Scan(&gotPayload))
		assert.Equal(t, c.payload, gotPayload,
			"payload bytes for %s must roundtrip exactly (key order, whitespace preserved)", c.eventType)
	}
}
