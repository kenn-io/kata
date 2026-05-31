package jsonl_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/uid"
)

func TestImportRoundTripsExportedRows(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)
	issue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "round trip",
		Author:    "tester",
		Labels:    []string{"bug"},
	})
	require.NoError(t, err)
	_, _, err = src.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "comment"})
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &exported, jsonl.ExportOptions{IncludeDeleted: true}))

	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(exported.Bytes()), dst))

	assertTableCount(t, src, dst, "projects")
	assertTableCount(t, src, dst, "issues")
	assertTableCount(t, src, dst, "comments")
	assertTableCount(t, src, dst, "issue_labels")
	assertTableCount(t, src, dst, "events")

	got, err := dst.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, issue.ShortID, got.ShortID)
	assert.Equal(t, issue.Title, got.Title)
}

func TestImportRoundTripsExportedLargeIssueBody(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)
	body := strings.Repeat("large-body-", 2*1024*1024)
	issue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "large round trip",
		Body:      body,
		Author:    "tester",
	})
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &exported, jsonl.ExportOptions{IncludeDeleted: true}))

	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(exported.Bytes()), dst))

	got, err := dst.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, body, got.Body)
}

func TestImportSQLiteSequenceUsesUpdateOrInsert(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	require.NoError(t, importJSONL(ctx, target,
		validExportVersion,
		`{"kind":"sqlite_sequence","data":{"name":"issues","seq":150}}`,
		`{"kind":"sqlite_sequence","data":{"name":"issues","seq":150}}`,
	))

	var rows int
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_sequence WHERE name='issues'`).Scan(&rows))
	assert.Equal(t, 1, rows)
	var seq int64
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name='issues'`).Scan(&seq))
	assert.Equal(t, int64(150), seq)
}

func TestImportFederationSyncStatus(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)
	require.NoError(t, importJSONL(ctx, target,
		`{"kind":"meta","data":{"key":"export_version","value":"12"}}`,
		`{"kind":"meta","data":{"key":"instance_uid","value":"01HZZZZZZZZZZZZZZZZZZZZZ10"}}`,
		`{"kind":"project","data":{"id":1,"uid":"01HZZZZZZZZZZZZZZZZZZZZZ11","name":"hub","metadata":{},"revision":1,"created_at":"2026-05-23T00:00:00.000Z"}}`,
		`{"kind":"federation_sync_status","data":{"project_id":1,"last_pull_started_at":"2026-05-23T01:00:00.000Z","last_pull_success_at":"2026-05-23T01:00:01.000Z","last_push_started_at":"2026-05-23T01:00:02.000Z","last_push_success_at":"2026-05-23T01:00:03.000Z","last_error_at":"2026-05-23T01:00:04.000Z","last_error":"hub offline","last_reset_at":"2026-05-23T01:00:05.000Z"}}`,
	))

	got, err := target.FederationSyncStatusByProject(ctx, 1)
	require.NoError(t, err)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:00.000Z"), got.LastPullStartedAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:01.000Z"), got.LastPullSuccessAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:02.000Z"), got.LastPushStartedAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:03.000Z"), got.LastPushSuccessAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:04.000Z"), got.LastErrorAt)
	assertTimePtrEqual(t, mustParseTime(t, "2026-05-23T01:00:05.000Z"), got.LastResetAt)
	require.NotNil(t, got.LastError)
	assert.Equal(t, "hub offline", *got.LastError)
}

func TestImportV11JSONLDefaultsWithoutFederationSyncStatus(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)
	require.NoError(t, importJSONL(ctx, target,
		`{"kind":"meta","data":{"key":"export_version","value":"11"}}`,
		`{"kind":"meta","data":{"key":"schema_version","value":"11"}}`,
		`{"kind":"meta","data":{"key":"instance_uid","value":"01HZZZZZZZZZZZZZZZZZZZZZ10"}}`,
		`{"kind":"project","data":{"id":1,"uid":"01HZZZZZZZZZZZZZZZZZZZZZ11","name":"hub","metadata":{},"revision":1,"created_at":"2026-05-23T00:00:00.000Z"}}`,
	))

	_, err := target.FederationSyncStatusByProject(ctx, 1)
	assert.ErrorIs(t, err, db.ErrNotFound)
	var schemaVersion string
	require.NoError(t, target.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&schemaVersion))
	assert.Equal(t, fmt.Sprint(db.CurrentSchemaVersion()), schemaVersion)
}

func TestImportV1FillsUIDsDeterministically(t *testing.T) {
	ctx := context.Background()
	rows := []string{
		validExportVersion,
		`{"kind":"project","data":{"id":1,"identity":"github.com/wesm/kata","name":"kata","created_at":"2026-05-03T00:00:00.000Z","next_issue_number":2}}`,
		`{"kind":"issue","data":{"id":1,"project_id":1,"number":1,"title":"v1 issue","body":"","status":"open","closed_reason":null,"owner":null,"author":"tester","created_at":"2026-05-03T00:00:01.000Z","updated_at":"2026-05-03T00:00:01.000Z","closed_at":null,"deleted_at":null}}`,
		`{"kind":"event","data":{"id":1,"project_id":1,"project_identity":"github.com/wesm/kata","issue_id":1,"issue_number":1,"related_issue_id":null,"type":"issue.created","actor":"tester","payload":{},"created_at":"2026-05-03T00:00:01.000Z"}}`,
	}

	first := openImportTargetDB(t)
	require.NoError(t, importJSONL(ctx, first, rows...))
	second := openImportTargetDB(t)
	require.NoError(t, importJSONL(ctx, second, rows...))

	firstUIDs := readFilledUIDs(t, first)
	secondUIDs := readFilledUIDs(t, second)
	assert.Equal(t, firstUIDs, secondUIDs)
	for _, got := range firstUIDs {
		assert.True(t, uid.Valid(got), "invalid uid %q", got)
	}
	var schemaVersion string
	require.NoError(t, first.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&schemaVersion))
	assert.Equal(t, fmt.Sprint(db.CurrentSchemaVersion()), schemaVersion)
}

func TestImportV1NormalizesGoStringIssueAndCommentTimestamps(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	require.NoError(t, importJSONL(ctx, target,
		validExportVersion,
		validV1ProjectRow,
		`{"kind":"issue","data":{"id":1,"project_id":1,"number":1,"title":"v1 issue","body":"","status":"open","closed_reason":null,"owner":null,"author":"tester","created_at":"2026-05-04 00:21:07 +0000 UTC","updated_at":"2026-05-04 00:21:07 +0000 UTC","closed_at":null,"deleted_at":null}}`,
		`{"kind":"comment","data":{"id":1,"issue_id":1,"author":"tester","body":"legacy note","created_at":"2026-05-04 00:21:07 +0000 UTC"}}`,
	))

	var commentUID string
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT uid FROM comments WHERE id = 1`).Scan(&commentUID))
	assert.True(t, uid.Valid(commentUID), "invalid filled comment uid %q", commentUID)

	issue, err := target.IssueByID(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "2026-05-04T00:21:07.000Z", issue.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"))
	assert.Equal(t, "2026-05-04T00:21:07.000Z", issue.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"))

	var issueCreatedAt, issueUpdatedAt, commentCreatedAt string
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM issues WHERE id = 1`,
	).Scan(&issueCreatedAt, &issueUpdatedAt))
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT CAST(created_at AS TEXT) FROM comments WHERE id = 1`,
	).Scan(&commentCreatedAt))
	assert.Equal(t, "2026-05-04T00:21:07.000Z", issueCreatedAt)
	assert.Equal(t, "2026-05-04T00:21:07.000Z", issueUpdatedAt)
	assert.Equal(t, "2026-05-04T00:21:07.000Z", commentCreatedAt)
}

func TestImportV1NormalizesFractionalGoStringTimestamps(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	require.NoError(t, importJSONL(ctx, target,
		validExportVersion,
		validV1ProjectRow,
		`{"kind":"issue","data":{"id":1,"project_id":1,"number":1,"title":"v1 issue","body":"","status":"open","closed_reason":null,"owner":null,"author":"tester","created_at":"2026-05-04 00:21:07.123 +0000 UTC","updated_at":"2026-05-04 00:21:07.123 +0000 UTC","closed_at":null,"deleted_at":null}}`,
		`{"kind":"comment","data":{"id":1,"issue_id":1,"author":"tester","body":"legacy note","created_at":"2026-05-04 00:21:07.123 +0000 UTC"}}`,
	))

	var issueCreatedAt, issueUpdatedAt, commentCreatedAt string
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM issues WHERE id = 1`,
	).Scan(&issueCreatedAt, &issueUpdatedAt))
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT CAST(created_at AS TEXT) FROM comments WHERE id = 1`,
	).Scan(&commentCreatedAt))
	assert.Equal(t, "2026-05-04T00:21:07.123Z", issueCreatedAt)
	assert.Equal(t, "2026-05-04T00:21:07.123Z", issueUpdatedAt)
	assert.Equal(t, "2026-05-04T00:21:07.123Z", commentCreatedAt)
}

func TestImportV1NormalizesGoStringEventTimestampForStats(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	require.NoError(t, importJSONL(ctx, target,
		validExportVersion,
		validV1ProjectRow,
		`{"kind":"issue","data":{"id":1,"project_id":1,"number":1,"title":"v1 issue","body":"","status":"open","closed_reason":null,"owner":null,"author":"tester","created_at":"2026-05-04T00:21:07.000Z","updated_at":"2026-05-04T00:21:07.000Z","closed_at":null,"deleted_at":null}}`,
		`{"kind":"event","data":{"id":1,"project_id":1,"project_identity":"github.com/wesm/kata","issue_id":1,"issue_number":1,"related_issue_id":null,"type":"issue.created","actor":"tester","payload":{},"created_at":"2026-05-04 00:21:07 +0000 UTC"}}`,
	))

	stats, err := target.BatchProjectStats(ctx)
	require.NoError(t, err)
	require.NotNil(t, stats[1].LastEventAt, "imported Go-string event timestamp must participate in stats")
	assert.Equal(t, "2026-05-04T00:21:07.000Z",
		stats[1].LastEventAt.UTC().Format("2006-01-02T15:04:05.000Z"))
}

func TestImportCurrentVersionRejectsEventHashForPreNormalizedTimestamp(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)
	projectUID := "01HZZZZZZZZZZZZZZZZZZZZZ11"
	eventUID := "01HZNQ7VFPK1XGD8R5MABCD4EX"
	originUID := "01HZNQ7VFPK1XGD8R5MABCD4EY"
	createdAt := "2026-05-04 00:21:07 +0000 UTC"
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               eventUID,
		OriginInstanceUID: originUID,
		ProjectUID:        projectUID,
		Type:              "project.imported",
		Actor:             "tester",
		HLCPhysicalMS:     1777854067000,
		HLCCounter:        1,
		CreatedAt:         createdAt,
		Payload:           []byte(`{}`),
	})
	require.NoError(t, err)

	err = importJSONL(ctx, target,
		fmt.Sprintf(`{"kind":"meta","data":{"key":"export_version","value":"%d"}}`, db.CurrentSchemaVersion()),
		`{"kind":"project","data":{"id":1,"uid":"`+projectUID+`","name":"kata","metadata":{},"revision":1,"created_at":"2026-05-04T00:00:00.000Z"}}`,
		`{"kind":"event","data":{"id":1,"uid":"`+eventUID+`","origin_instance_uid":"`+originUID+`","project_id":1,"project_name":"kata","issue_id":null,"issue_uid":null,"related_issue_id":null,"related_issue_uid":null,"type":"project.imported","actor":"tester","payload":{},"hlc_physical_ms":1777854067000,"hlc_counter":1,"content_hash":"`+hash+`","created_at":"`+createdAt+`"}}`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "content_hash")
}

func TestImportLegacyEventSnapshotsUseFinalProjectName(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)
	_, err := target.CreateProject(ctx, "kata")
	require.NoError(t, err)

	require.NoError(t, importJSONL(ctx, target,
		validExportVersion,
		`{"kind":"project","data":{"id":3,"identity":"github.com/example/kata","name":"kata","created_at":"2026-05-03T00:00:00.000Z","next_issue_number":2}}`,
		`{"kind":"issue","data":{"id":1,"project_id":3,"number":1,"title":"legacy issue","body":"","status":"open","closed_reason":null,"owner":null,"author":"tester","created_at":"2026-05-03T00:00:01.000Z","updated_at":"2026-05-03T00:00:01.000Z","closed_at":null,"deleted_at":null}}`,
		`{"kind":"event","data":{"id":1,"project_id":3,"project_identity":"github.com/example/kata","issue_id":1,"issue_number":1,"related_issue_id":null,"type":"issue.created","actor":"tester","payload":{},"created_at":"2026-05-03T00:00:01.000Z"}}`,
	))

	var projectName, eventProjectName string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = 3`).Scan(&projectName))
	require.NoError(t, target.QueryRowContext(ctx, `SELECT project_name FROM events WHERE id = 1`).Scan(&eventProjectName))
	assert.Equal(t, "kata-2", projectName)
	assert.Equal(t, projectName, eventProjectName)
}

func TestImportV1RejectsCorruptEventFK(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	err := importJSONL(ctx, target,
		validExportVersion,
		validV1ProjectRow,
		`{"kind":"event","data":{"id":7,"project_id":1,"project_identity":"github.com/wesm/kata","issue_id":999,"issue_number":1,"related_issue_id":null,"type":"issue.created","actor":"tester","payload":{},"created_at":"2026-05-03T00:00:01.000Z"}}`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corrupt_event_fk")
	assert.Contains(t, err.Error(), "event 7 issue_id 999")
}

func TestImportRejectsInvalidExportVersion(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	err := importJSONL(ctx, target,
		`{"kind":"meta","data":{"key":"export_version","value":"not-a-version"}}`,
		validV1ProjectRow,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "export_version")
	assertTableEmpty(t, target, "projects")
}

func TestImportRejectsTooNewExportVersion(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	err := importJSONL(ctx, target,
		`{"kind":"meta","data":{"key":"export_version","value":"999"}}`,
		validV1ProjectRow,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported export_version")
	assertTableEmpty(t, target, "projects")
}

func TestImportRejectsForeignKeyViolationBeforeCommit(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	err := importJSONL(ctx, target,
		validExportVersion,
		`{"kind":"project_alias","data":{"id":1,"project_id":999,"alias_identity":"missing","alias_kind":"git","root_path":"/tmp/missing","created_at":"2026-05-03T00:00:00.000Z","last_seen_at":"2026-05-03T00:00:00.000Z"}}`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "foreign_key_check")
	assert.Contains(t, err.Error(), "project_aliases rowid=1 parent=projects")
	var count int
	require.NoError(t, target.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_aliases`).Scan(&count))
	assert.Equal(t, 0, count)
}

func TestImportRejectsForeignKeyViolationsListsEveryRow(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	// Two project_alias rows each referencing a different missing project.
	// Verifies that all violation rows are listed in the error, not just the
	// first. (Cross-table coverage is not possible here: the comments table
	// has an AFTER INSERT trigger that rewrites issues_fts synchronously, so
	// inserting an orphan comment corrupts the FTS state before deferred FK
	// checks fire. project_aliases has no such trigger, so multiple orphan
	// rows of the same table are the cleanest multi-row coverage available.)
	err := importJSONL(ctx, target,
		validExportVersion,
		`{"kind":"project_alias","data":{"id":1,"project_id":777,"alias_identity":"missing-a","alias_kind":"git","root_path":"/tmp/a","created_at":"2026-05-03T00:00:00.000Z","last_seen_at":"2026-05-03T00:00:00.000Z"}}`,
		`{"kind":"project_alias","data":{"id":2,"project_id":888,"alias_identity":"missing-b","alias_kind":"git","root_path":"/tmp/b","created_at":"2026-05-03T00:00:00.000Z","last_seen_at":"2026-05-03T00:00:00.000Z"}}`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "foreign_key_check")
	assert.Contains(t, err.Error(), "project_aliases rowid=1 parent=projects column=project_id")
	assert.Contains(t, err.Error(), "project_aliases rowid=2 parent=projects column=project_id")
	var count int
	require.NoError(t, target.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_aliases`).Scan(&count))
	assert.Equal(t, 0, count)
}

func TestImportRejectsTokenCreatedMissingRequiredFields(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	err := importJSONL(ctx, target,
		`{"kind":"meta","data":{"key":"export_version","value":"11"}}`,
		`{"kind":"project","data":{"id":1,"uid":"00000000000000000000000000","name":".kata-system","created_at":"2026-05-03T00:00:00.000Z","metadata":{},"revision":1}}`,
		`{"kind":"event","data":{"id":1,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","origin_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EY","project_id":1,"project_name":".kata-system","issue_id":null,"issue_uid":null,"related_issue_id":null,"related_issue_uid":null,"type":"token.created","actor":"bootstrap","payload":{"token_id":1,"target_actor":"alice"},"created_at":"2026-05-03T00:00:01.000Z"}}`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "token.created")
	assert.Contains(t, err.Error(), "missing required field")
}

func TestImportRejectsTokenRevokedForUnknownToken(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	err := importJSONL(ctx, target,
		`{"kind":"meta","data":{"key":"export_version","value":"11"}}`,
		`{"kind":"project","data":{"id":1,"uid":"00000000000000000000000000","name":".kata-system","created_at":"2026-05-03T00:00:00.000Z","metadata":{},"revision":1}}`,
		`{"kind":"event","data":{"id":1,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","origin_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EY","project_id":1,"project_name":".kata-system","issue_id":null,"issue_uid":null,"related_issue_id":null,"related_issue_uid":null,"type":"token.revoked","actor":"bootstrap","payload":{"token_id":99},"created_at":"2026-05-03T00:00:01.000Z"}}`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "token.revoked 99")
	assert.Contains(t, err.Error(), "token not found")
}

func TestImportRejectsTokenEventsOutsideSystemProject(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	err := importJSONL(ctx, target,
		`{"kind":"meta","data":{"key":"export_version","value":"11"}}`,
		`{"kind":"project","data":{"id":1,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EZ","name":"normal","created_at":"2026-05-03T00:00:00.000Z","metadata":{},"revision":1}}`,
		`{"kind":"event","data":{"id":1,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","origin_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EY","project_id":1,"project_name":"normal","issue_id":null,"issue_uid":null,"related_issue_id":null,"related_issue_uid":null,"type":"token.created","actor":"bootstrap","payload":{"token_id":1,"token_hash":"`+db.HashTokenForTest("evil-token")+`","target_actor":"mallory"},"created_at":"2026-05-03T00:00:01.000Z"}}`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "token.created")
	assert.Contains(t, err.Error(), "system project")
}

func TestImportRejectsMalformedTokenReplayFields(t *testing.T) {
	for _, tc := range []struct {
		name        string
		payload     string
		wantMessage string
	}{
		{
			name:        "invalid hash",
			payload:     `{"token_id":1,"token_hash":"not-hex","target_actor":"alice"}`,
			wantMessage: "token_hash",
		},
		{
			name:        "reserved actor",
			payload:     `{"token_id":1,"token_hash":"` + db.HashTokenForTest("secret-token") + `","target_actor":"bootstrap"}`,
			wantMessage: "reserved",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			target := openImportTargetDB(t)

			err := importJSONL(ctx, target,
				`{"kind":"meta","data":{"key":"export_version","value":"11"}}`,
				`{"kind":"project","data":{"id":1,"uid":"00000000000000000000000000","name":".kata-system","created_at":"2026-05-03T00:00:00.000Z","metadata":{},"revision":1}}`,
				`{"kind":"event","data":{"id":1,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","origin_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EY","project_id":1,"project_name":".kata-system","issue_id":null,"issue_uid":null,"related_issue_id":null,"related_issue_uid":null,"type":"token.created","actor":"bootstrap","payload":`+tc.payload+`,"created_at":"2026-05-03T00:00:01.000Z"}}`,
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMessage)
		})
	}
}

func openImportTargetDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func assertTableCount(t *testing.T, src, dst *db.DB, table string) {
	t.Helper()
	var want, got int
	require.NoError(t, src.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&want))
	require.NoError(t, dst.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&got))
	assert.Equal(t, want, got, table)
}

func assertTableEmpty(t *testing.T, d *db.DB, table string) {
	t.Helper()
	var got int
	query := `SELECT COUNT(*) FROM ` + table
	if table == "projects" {
		query += ` WHERE name <> '` + db.SystemProjectName + `'`
	}
	require.NoError(t, d.QueryRow(query).Scan(&got))
	assert.Equal(t, 0, got, table)
}

func readFilledUIDs(t *testing.T, d *db.DB) []string {
	t.Helper()
	var projectUID, issueUID, eventIssueUID string
	require.NoError(t, d.QueryRow(`SELECT uid FROM projects WHERE id = 1`).Scan(&projectUID))
	require.NoError(t, d.QueryRow(`SELECT uid FROM issues WHERE id = 1`).Scan(&issueUID))
	require.NoError(t, d.QueryRow(`SELECT issue_uid FROM events WHERE id = 1`).Scan(&eventIssueUID))
	return []string{projectUID, issueUID, eventIssueUID}
}
