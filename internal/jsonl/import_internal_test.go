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
				require.NotNil(t, rec.Project)
				assert.Equal(t, canonical, rec.Project.CreatedAt)
				assert.Equal(t, canonical, *rec.Project.DeletedAt)
			},
		},
		{
			name: "alias",
			kind: KindProjectAlias,
			data: `{"id":1,"project_id":1,"alias_identity":"repo","alias_kind":"git","root_path":"/tmp/repo","created_at":"` + legacy + `","last_seen_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.Alias)
				assert.Equal(t, canonical, rec.Alias.CreatedAt)
				assert.Equal(t, canonical, rec.Alias.LastSeenAt)
			},
		},
		{
			name: "recurrence",
			kind: KindRecurrence,
			data: `{"id":1,"uid":"` + otherUID + `","project_id":1,"rrule":"FREQ=DAILY","dtstart":"2026-05-04T00:00:00.000Z","timezone":"UTC","template_title":"todo","template_body":"","template_labels":[],"template_metadata":{},"author":"tester","revision":1,"created_at":"` + legacy + `","updated_at":"` + legacy + `","deleted_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.Recurrence)
				assert.Equal(t, canonical, rec.Recurrence.CreatedAt)
				assert.Equal(t, canonical, rec.Recurrence.UpdatedAt)
				assert.Equal(t, canonical, *rec.Recurrence.DeletedAt)
			},
		},
		{
			name: "label",
			kind: KindIssueLabel,
			data: `{"issue_id":1,"label":"bug","author":"tester","created_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.Label)
				assert.Equal(t, canonical, rec.Label.CreatedAt)
			},
		},
		{
			name: "link",
			kind: KindLink,
			data: `{"id":1,"project_id":1,"from_issue_id":1,"from_issue_uid":"` + issueUID + `","to_issue_id":2,"to_issue_uid":"` + otherUID + `","type":"blocks","author":"tester","created_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.Link)
				assert.Equal(t, canonical, rec.Link.CreatedAt)
			},
		},
		{
			name: "import mapping",
			kind: KindImportMapping,
			data: `{"id":1,"source":"gh","external_id":"1","object_type":"issue","project_id":1,"issue_id":1,"source_updated_at":"` + legacy + `","imported_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.ImportMapping)
				assert.Equal(t, canonical, *rec.ImportMapping.SourceUpdatedAt)
				assert.Equal(t, canonical, rec.ImportMapping.ImportedAt)
			},
		},
		{
			name: "federation binding",
			kind: KindFederationBinding,
			data: `{"project_id":1,"role":"hub","hub_url":"","hub_project_id":0,"hub_project_uid":"` + projectUID + `","replay_horizon_event_id":0,"pull_cursor_event_id":0,"push_enabled":false,"push_cursor_event_id":0,"enabled":true,"created_at":"` + legacy + `","updated_at":"` + legacy + `","last_sync_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.FederationBinding)
				assert.Equal(t, canonical, rec.FederationBinding.CreatedAt)
				assert.Equal(t, canonical, rec.FederationBinding.UpdatedAt)
				assert.Equal(t, canonical, *rec.FederationBinding.LastSyncAt)
			},
		},
		{
			name: "federation sync status",
			kind: KindFederationSyncStatus,
			data: `{"project_id":1,"last_pull_started_at":"` + legacy + `","last_pull_success_at":"` + legacy + `","last_push_started_at":"` + legacy + `","last_push_success_at":"` + legacy + `","last_error_at":"` + legacy + `","last_reset_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.FederationSyncStatus)
				assert.Equal(t, canonical, *rec.FederationSyncStatus.LastPullStartedAt)
				assert.Equal(t, canonical, *rec.FederationSyncStatus.LastPullSuccessAt)
				assert.Equal(t, canonical, *rec.FederationSyncStatus.LastPushStartedAt)
				assert.Equal(t, canonical, *rec.FederationSyncStatus.LastPushSuccessAt)
				assert.Equal(t, canonical, *rec.FederationSyncStatus.LastErrorAt)
				assert.Equal(t, canonical, *rec.FederationSyncStatus.LastResetAt)
			},
		},
		{
			name: "federation quarantine",
			kind: KindFederationQuarantine,
			data: `{"id":1,"project_id":1,"direction":"pull","first_event_id":1,"last_event_id":1,"event_uids":[],"error":"bad","created_at":"` + legacy + `","skipped_at":"` + legacy + `","skipped_by":"tester","skip_reason":"done"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.FederationQuarantine)
				assert.Equal(t, canonical, rec.FederationQuarantine.CreatedAt)
				assert.Equal(t, canonical, *rec.FederationQuarantine.SkippedAt)
			},
		},
		{
			name: "federation enrollment",
			kind: KindFederationEnrollment,
			data: `{"id":1,"token_hash":"` + stringOf("a", 64) + `","spoke_instance_uid":"` + otherUID + `","project_id":1,"capabilities":"pull","created_at":"` + legacy + `","updated_at":"` + legacy + `","revoked_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.FederationEnrollment)
				assert.Equal(t, canonical, rec.FederationEnrollment.CreatedAt)
				assert.Equal(t, canonical, rec.FederationEnrollment.UpdatedAt)
				assert.Equal(t, canonical, *rec.FederationEnrollment.RevokedAt)
			},
		},
		{
			name: "issue claim",
			kind: KindIssueClaim,
			data: `{"id":1,"claim_uid":"` + otherUID + `","project_id":1,"issue_id":1,"issue_uid":"` + issueUID + `","holder":"tester","holder_instance_uid":"` + projectUID + `","client_kind":"cli","purpose":"edit","claim_kind":"timed","acquired_at":"` + legacy + `","expires_at":"` + legacy + `","released_at":"` + legacy + `","release_reason":"done","revision":1,"updated_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.IssueClaim)
				assert.Equal(t, canonical, rec.IssueClaim.AcquiredAt)
				assert.Equal(t, canonical, *rec.IssueClaim.ExpiresAt)
				assert.Equal(t, canonical, *rec.IssueClaim.ReleasedAt)
				assert.Equal(t, canonical, rec.IssueClaim.UpdatedAt)
			},
		},
		{
			name: "pending claim request",
			kind: KindPendingClaimRequest,
			data: `{"id":1,"request_uid":"` + otherUID + `","project_id":1,"issue_id":1,"issue_uid":"` + issueUID + `","holder":"tester","holder_instance_uid":"` + projectUID + `","client_kind":"cli","claim_kind":"timed","ttl_seconds":60,"purpose":"edit","requested_at":"` + legacy + `","last_attempt_at":"` + legacy + `","last_error":"bad","rejected_at":"` + legacy + `","resolved_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.PendingClaimRequest)
				assert.Equal(t, canonical, rec.PendingClaimRequest.RequestedAt)
				assert.Equal(t, canonical, *rec.PendingClaimRequest.LastAttemptAt)
				assert.Equal(t, canonical, *rec.PendingClaimRequest.RejectedAt)
				assert.Equal(t, canonical, *rec.PendingClaimRequest.ResolvedAt)
			},
		},
		{
			name: "purge log",
			kind: KindPurgeLog,
			data: `{"id":1,"uid":"` + otherUID + `","origin_instance_uid":"` + projectUID + `","project_id":1,"purged_issue_id":1,"issue_uid":"` + issueUID + `","project_uid":"` + projectUID + `","project_name":"kata","short_id":"abc1","issue_title":"done","issue_author":"tester","comment_count":0,"link_count":0,"label_count":0,"event_count":0,"actor":"tester","reason":"done","purged_at":"` + legacy + `"}`,
			assert: func(t *testing.T, rec db.ImportRecord) {
				require.NotNil(t, rec.PurgeLog)
				assert.Equal(t, canonical, rec.PurgeLog.PurgedAt)
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

func stringOf(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
