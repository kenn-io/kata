package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

// TestImportKindMatchesWireConstant pins the type -> NDJSON kind mapping for
// every replay payload. The kind strings are frozen wire vocabulary shared
// with internal/jsonl, and after ImportRecord became an interface this table
// is the only place the whole mapping is asserted at once.
func TestImportKindMatchesWireConstant(t *testing.T) {
	cases := []struct {
		record db.ImportRecord
		want   string
	}{
		{&db.MetaKV{}, db.ImportKindMeta},
		{&db.ProjectExport{}, db.ImportKindProject},
		{&db.AliasExport{}, db.ImportKindProjectAlias},
		{&db.IssueSyncBindingExport{}, db.ImportKindIssueSyncBinding},
		{&db.IssueSyncStatusExport{}, db.ImportKindIssueSyncStatus},
		{&db.RecurrenceExport{}, db.ImportKindRecurrence},
		{&db.IssueExport{}, db.ImportKindIssue},
		{&db.IssueEmbeddingExport{}, db.ImportKindIssueEmbedding},
		{&db.CommentExport{}, db.ImportKindComment},
		{&db.IssueLabelExport{}, db.ImportKindIssueLabel},
		{&db.LinkExport{}, db.ImportKindLink},
		{&db.ImportMappingExport{}, db.ImportKindImportMapping},
		{&db.ExternalFieldMappingExport{}, db.ImportKindExternalFieldMapping},
		{&db.ExternalRootBindingExport{}, db.ImportKindExternalRootBinding},
		{&db.ExternalFieldStateExport{}, db.ImportKindExternalFieldState},
		{&db.FederationBindingExport{}, db.ImportKindFederationBinding},
		{&db.FederationSyncStatusExport{}, db.ImportKindFederationSyncStatus},
		{&db.FederationQuarantineExport{}, db.ImportKindFederationQuarantine},
		{&db.FederationEnrollmentExport{}, db.ImportKindFederationEnrollment},
		{&db.IssueClaimExport{}, db.ImportKindIssueClaim},
		{&db.PendingClaimRequestExport{}, db.ImportKindPendingClaimRequest},
		{&db.EventExport{}, db.ImportKindEvent},
		{&db.PurgeLogExport{}, db.ImportKindPurgeLog},
		{&db.ProjectPurgeLogExport{}, db.ImportKindProjectPurgeLog},
		{&db.SequenceExport{}, db.ImportKindSQLiteSequence},
	}

	assert.Len(t, cases, 25, "every replay payload type must be listed")
	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.record.ImportKind())
		})
		if prior, dup := seen[tc.want]; dup {
			t.Errorf("kind %q claimed by two types (%s)", tc.want, prior)
		}
		seen[tc.want] = tc.want
	}
	assert.Len(t, seen, 25, "the type -> kind mapping must be a bijection")
}
