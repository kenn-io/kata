package jsonl

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestToImportRecordNormalizesTimestampFields(t *testing.T) {
	const (
		legacy    = "2026-05-04 00:21:07 +0000 UTC"
		canonical = "2026-05-04T00:21:07.000Z"
	)

	projectUID := "01HZZZZZZZZZZZZZZZZZZZZZ11"
	issueUID := "01HZZZZZZZZZZZZZZZZZZZZZ12"
	otherUID := "01HZZZZZZZZZZZZZZZZZZZZZ13"

	cases := []struct {
		name   string
		kind   Kind
		data   string
		assert func(*testing.T, db.ImportRecord)
	}{
		{
			name: "project",
			kind: KindProject,
			data: `{"id":1,"uid":"` + projectUID + `","name":"kata","created_at":"` + legacy + `","deleted_at":"` + legacy + `","metadata":{},"revision":1}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				project := recordAs[db.ProjectExport](t, rec)
				assert.Equal(t, canonical, project.CreatedAt)
				assert.Equal(t, canonical, *project.DeletedAt)
			},
		},
		{
			name: "alias",
			kind: KindProjectAlias,
			data: `{"id":1,"project_id":1,"alias_identity":"repo","alias_kind":"git","root_path":"/tmp/repo","created_at":"` + legacy + `","last_seen_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				alias := recordAs[db.AliasExport](t, rec)
				assert.Equal(t, canonical, alias.CreatedAt)
			},
		},
		{
			name: "recurrence",
			kind: KindRecurrence,
			data: `{"id":1,"uid":"` + otherUID + `","project_id":1,"rrule":"FREQ=DAILY","dtstart":"2026-05-04T00:00:00.000Z","timezone":"UTC","template_title":"todo","template_body":"","template_labels":[],"template_metadata":{},"author":"tester","revision":1,"created_at":"` + legacy + `","updated_at":"` + legacy + `","deleted_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				recurrence := recordAs[db.RecurrenceExport](t, rec)
				assert.Equal(t, canonical, recurrence.CreatedAt)
				assert.Equal(t, canonical, recurrence.UpdatedAt)
				assert.Equal(t, canonical, *recurrence.DeletedAt)
			},
		},
		{
			name: "label",
			kind: KindIssueLabel,
			data: `{"issue_id":1,"label":"bug","author":"tester","created_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				label := recordAs[db.IssueLabelExport](t, rec)
				assert.Equal(t, canonical, label.CreatedAt)
			},
		},
		{
			name: "comment keeps microsecond precision",
			kind: KindComment,
			data: `{"id":1,"uid":"` + otherUID + `","issue_id":1,"author":"tester","body":"note","created_at":"2026-05-04T00:21:07.000500Z"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				comment := recordAs[db.CommentExport](t, rec)
				assert.Equal(t, "2026-05-04T00:21:07.000500Z", comment.CreatedAt)
			},
		},
		{
			name: "comment keeps nanosecond precision",
			kind: KindComment,
			data: `{"id":2,"uid":"` + projectUID + `","issue_id":1,"author":"tester","body":"note","created_at":"2026-05-04T00:21:07.000500900Z"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				comment := recordAs[db.CommentExport](t, rec)
				assert.Equal(t, "2026-05-04T00:21:07.000500900Z", comment.CreatedAt)
			},
		},
		{
			name: "comment canonicalizes offset microsecond stamps",
			kind: KindComment,
			data: `{"id":3,"uid":"` + issueUID + `","issue_id":1,"author":"tester","body":"note","created_at":"2026-05-04T02:21:07.000500+02:00"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				comment := recordAs[db.CommentExport](t, rec)
				assert.Equal(t, "2026-05-04T00:21:07.000500Z", comment.CreatedAt)
			},
		},
		{
			name: "comment canonicalizes variable-length fractions",
			kind: KindComment,
			data: `{"id":4,"uid":"` + otherUID + `","issue_id":1,"author":"tester","body":"note","created_at":"2026-05-04T00:21:07.0005Z"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				comment := recordAs[db.CommentExport](t, rec)
				assert.Equal(t, "2026-05-04T00:21:07.000500Z", comment.CreatedAt)
			},
		},
		{
			name: "comment canonicalizes legacy nanosecond stamps",
			kind: KindComment,
			data: `{"id":5,"uid":"` + projectUID + `","issue_id":1,"author":"tester","body":"note","created_at":"2026-05-04 00:21:07.0005009 +0000 UTC"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				comment := recordAs[db.CommentExport](t, rec)
				assert.Equal(t, "2026-05-04T00:21:07.000500900Z", comment.CreatedAt)
			},
		},
		{
			name: "link",
			kind: KindLink,
			data: `{"id":1,"project_id":1,"from_issue_id":1,"from_issue_uid":"` + issueUID + `","to_issue_id":2,"to_issue_uid":"` + otherUID + `","type":"blocks","author":"tester","created_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				link := recordAs[db.LinkExport](t, rec)
				assert.Equal(t, canonical, link.CreatedAt)
			},
		},
		{
			name: "import mapping",
			kind: KindImportMapping,
			data: `{"id":1,"source":"gh","external_id":"1","object_type":"issue","project_id":1,"issue_id":1,"source_updated_at":"` + legacy + `","imported_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				mapping := recordAs[db.ImportMappingExport](t, rec)
				assert.Equal(t, canonical, *mapping.SourceUpdatedAt)
				assert.Equal(t, canonical, mapping.ImportedAt)
			},
		},
		{
			name: "federation binding",
			kind: KindFederationBinding,
			data: `{"project_id":1,"role":"hub","hub_url":"","hub_project_id":0,"hub_project_uid":"` + projectUID + `","replay_horizon_event_id":0,"pull_cursor_event_id":0,"push_enabled":false,"push_cursor_event_id":0,"enabled":true,"created_at":"` + legacy + `","updated_at":"` + legacy + `","last_sync_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				binding := recordAs[db.FederationBindingExport](t, rec)
				assert.Equal(t, canonical, binding.CreatedAt)
				assert.Equal(t, canonical, binding.UpdatedAt)
				assert.Equal(t, canonical, *binding.LastSyncAt)
			},
		},
		{
			name: "federation sync status",
			kind: KindFederationSyncStatus,
			data: `{"project_id":1,"last_pull_started_at":"` + legacy + `","last_pull_success_at":"` + legacy + `","last_push_started_at":"` + legacy + `","last_push_success_at":"` + legacy + `","last_error_at":"` + legacy + `","last_reset_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				status := recordAs[db.FederationSyncStatusExport](t, rec)
				assert.Equal(t, canonical, *status.LastPullStartedAt)
				assert.Equal(t, canonical, *status.LastPullSuccessAt)
				assert.Equal(t, canonical, *status.LastPushStartedAt)
				assert.Equal(t, canonical, *status.LastPushSuccessAt)
				assert.Equal(t, canonical, *status.LastErrorAt)
				assert.Equal(t, canonical, *status.LastResetAt)
			},
		},
		{
			name: "github sync binding",
			kind: KindIssueSyncBinding,
			data: `{"id":1,"project_id":1,"source_key":"github:repo-node-example","host":"github.com","owner":"example-org","repo":"example-repo","repo_node_id":"repo-node-example","repo_id":42,"enabled":false,"interval_seconds":900,"last_cursor_at":"` + legacy + `","created_at":"` + legacy + `","updated_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				binding := recordAs[db.IssueSyncBindingExport](t, rec)
				assert.Equal(t, canonical, *binding.LastCursorAt)
				assert.Equal(t, canonical, binding.CreatedAt)
				assert.Equal(t, canonical, binding.UpdatedAt)
			},
		},
		{
			name: "github sync status",
			kind: KindIssueSyncStatus,
			data: `{"binding_id":1,"project_id":1,"sync_started_at":"` + legacy + `","last_attempt_at":"` + legacy + `","last_success_at":"` + legacy + `","last_error_at":"` + legacy + `","last_error":"rate limited","last_created":2,"last_updated":3,"last_unchanged":4,"last_comments":5}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				status := recordAs[db.IssueSyncStatusExport](t, rec)
				assert.Equal(t, canonical, *status.SyncStartedAt)
				assert.Equal(t, canonical, *status.LastAttemptAt)
				assert.Equal(t, canonical, *status.LastSuccessAt)
				assert.Equal(t, canonical, *status.LastErrorAt)
				require.NotNil(t, status.LastError)
				assert.Equal(t, "rate limited", *status.LastError)
				assert.Equal(t, 2, status.LastCreated)
				assert.Equal(t, 3, status.LastUpdated)
				assert.Equal(t, 4, status.LastUnchanged)
				assert.Equal(t, 5, status.LastComments)
			},
		},
		{
			name: "federation quarantine",
			kind: KindFederationQuarantine,
			data: `{"id":1,"project_id":1,"direction":"pull","first_event_id":1,"last_event_id":1,"event_uids":[],"error":"bad","created_at":"` + legacy + `","skipped_at":"` + legacy + `","skipped_by":"tester","skip_reason":"done"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				quarantine := recordAs[db.FederationQuarantineExport](t, rec)
				assert.Equal(t, canonical, quarantine.CreatedAt)
				assert.Equal(t, canonical, *quarantine.SkippedAt)
			},
		},
		{
			name: "federation enrollment",
			kind: KindFederationEnrollment,
			data: `{"id":1,"token_hash":"` + stringOf("a", 64) + `","spoke_instance_uid":"` + otherUID + `","project_id":1,"capabilities":"pull","created_at":"` + legacy + `","updated_at":"` + legacy + `","revoked_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				enrollment := recordAs[db.FederationEnrollmentExport](t, rec)
				assert.Equal(t, canonical, enrollment.CreatedAt)
				assert.Equal(t, canonical, enrollment.UpdatedAt)
				assert.Equal(t, canonical, *enrollment.RevokedAt)
			},
		},
		{
			name: "issue claim",
			kind: KindIssueClaim,
			data: `{"id":1,"claim_uid":"` + otherUID + `","project_id":1,"issue_id":1,"issue_uid":"` + issueUID + `","holder":"tester","holder_instance_uid":"` + projectUID + `","client_kind":"cli","purpose":"edit","claim_kind":"timed","acquired_at":"` + legacy + `","expires_at":"` + legacy + `","released_at":"` + legacy + `","release_reason":"done","revision":1,"updated_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				claim := recordAs[db.IssueClaimExport](t, rec)
				assert.Equal(t, canonical, claim.AcquiredAt)
				assert.Equal(t, canonical, *claim.ExpiresAt)
				assert.Equal(t, canonical, *claim.ReleasedAt)
				assert.Equal(t, canonical, claim.UpdatedAt)
			},
		},
		{
			name: "pending claim request",
			kind: KindPendingClaimRequest,
			data: `{"id":1,"request_uid":"` + otherUID + `","project_id":1,"issue_id":1,"issue_uid":"` + issueUID + `","holder":"tester","holder_instance_uid":"` + projectUID + `","client_kind":"cli","claim_kind":"timed","ttl_seconds":60,"purpose":"edit","requested_at":"` + legacy + `","last_attempt_at":"` + legacy + `","last_error":"bad","rejected_at":"` + legacy + `","resolved_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				request := recordAs[db.PendingClaimRequestExport](t, rec)
				assert.Equal(t, canonical, request.RequestedAt)
				assert.Equal(t, canonical, *request.LastAttemptAt)
				assert.Equal(t, canonical, *request.RejectedAt)
				assert.Equal(t, canonical, *request.ResolvedAt)
			},
		},
		{
			name: "purge log",
			kind: KindPurgeLog,
			data: `{"id":1,"uid":"` + otherUID + `","origin_instance_uid":"` + projectUID + `","project_id":1,"purged_issue_id":1,"issue_uid":"` + issueUID + `","project_uid":"` + projectUID + `","project_name":"kata","short_id":"abc1","issue_title":"done","issue_author":"tester","comment_count":0,"link_count":0,"label_count":0,"event_count":0,"actor":"tester","reason":"done","purged_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				purgeLog := recordAs[db.PurgeLogExport](t, rec)
				assert.Equal(t, canonical, purgeLog.PurgedAt)
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage = []byte(tt.data)
			rec, err := toImportRecord(
				Envelope{Kind: tt.kind, Data: raw},
				db.CurrentSchemaVersion(),
				projectUID,
				map[int64]string{1: projectUID},
			)
			require.NoError(t, err)
			tt.assert(t, rec)
		})
	}
}

func TestKindOrderPlacesGitHubSyncAfterProjectAlias(t *testing.T) {
	assert.Equal(t, kindOrder[KindProjectAlias]+1, kindOrder[KindIssueSyncBinding])
	assert.Equal(t, kindOrder[KindIssueSyncBinding]+1, kindOrder[KindIssueSyncStatus])
	assert.Equal(t, kindOrder[KindIssueSyncStatus]+1, kindOrder[KindRecurrence])
}

func stringOf(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// recordAs asserts the mapped record's payload type and returns it, replacing
// the old `require.NotNil(t, rec.Field)` guard now that the discriminator is
// the type itself.
func recordAs[T any](t *testing.T, rec db.ImportRecord) *T {
	t.Helper()
	got, ok := any(rec).(*T)
	require.True(t, ok, "record is %T, want *%T", rec, *new(T))
	require.NotNil(t, got)
	return got
}
