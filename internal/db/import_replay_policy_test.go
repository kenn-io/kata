package db_test

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kata/internal/db"
)

// unknownRecord implements db.ImportRecord but is not one of the 22 replay
// payloads. ValidateImportRecords must refuse it: the backends' type switches
// would otherwise reach their default arm inside an open transaction.
type unknownRecord struct{}

func (unknownRecord) ImportKind() string { return "totally-unknown" }

func TestValidateImportRecordsRejectsMalformed(t *testing.T) {
	valid := &db.MetaKV{Key: "instance_uid", Value: "01KATA00000000000000000001"}

	cases := []struct {
		name    string
		records []db.ImportRecord
		wantErr string
	}{
		{
			name:    "nil interface",
			records: []db.ImportRecord{valid, nil},
			wantErr: "import record 1",
		},
		{
			name:    "typed nil payload",
			records: []db.ImportRecord{valid, (*db.ProjectExport)(nil)},
			wantErr: "nil payload",
		},
		{
			name:    "type outside the replay union",
			records: []db.ImportRecord{valid, unknownRecord{}},
			wantErr: "unknown record type",
		},
		{
			name: "invalid comment display contribution",
			records: []db.ImportRecord{valid, &db.CommentExport{
				UID: "01KATA00000000000000000001", AttributedTeammate: new("bad/handle"),
			}},
			wantErr: "attributed_teammate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := db.ValidateImportRecords(tc.records)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "import record 1",
				"the error must name the slice ordinal")
		})
	}
}

func TestValidateImportRecordsAcceptsLegacyCommentEventPayload(t *testing.T) {
	payload := jsontext.Value(`{"comment_id":42}`)
	contentHash := strings.Repeat("a", 64)
	record := &db.EventExport{
		UID:         "legacy-event",
		Type:        "issue.commented",
		Payload:     payload,
		ContentHash: contentHash,
	}
	require.NoError(t, db.ValidateImportRecords([]db.ImportRecord{record}))
	require.Equal(t, payload, record.Payload, "import validation must not rewrite the historical event payload")
	require.Equal(t, contentHash, record.ContentHash, "import validation must leave the historical content hash untouched")
}

func TestValidateImportRecordsAcceptsEveryPayloadType(t *testing.T) {
	mappingTime := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	records := []db.ImportRecord{
		&db.MetaKV{}, &db.ProjectExport{}, &db.AliasExport{},
		&db.IssueSyncBindingExport{}, &db.IssueSyncStatusExport{},
		&db.RecurrenceExport{}, &db.IssueExport{}, &db.IssueEmbeddingExport{},
		&db.CommentExport{UID: "01KATA00000000000000000001", AttributedTeammate: new("")},
		&db.IssueLabelExport{}, &db.LinkExport{},
		&db.ImportMappingExport{}, &db.ExternalFieldMappingExport{
			ConnectorInstance: "connector-one", KataField: "scheduled_on",
			ExternalFieldID: "schedule-one", ExternalFieldName: "Schedule",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-one", CreatedAt: mappingTime, UpdatedAt: mappingTime,
		},
		&db.ExternalRootBindingExport{}, &db.ExternalFieldStateExport{},
		&db.FederationBindingExport{},
		&db.FederationSyncStatusExport{}, &db.FederationQuarantineExport{},
		&db.FederationEnrollmentExport{}, &db.IssueClaimExport{},
		&db.PendingClaimRequestExport{}, &db.EventExport{},
		&db.PurgeLogExport{}, &db.ProjectPurgeLogExport{}, &db.SequenceExport{},
	}
	require.Len(t, records, 25)
	assert.NoError(t, db.ValidateImportRecords(records))
}

func TestValidateImportRecordsRejectsCanonicalCommentUIDDuplicates(t *testing.T) {
	uid := "01ARZ3NDEKTSV4RRFFQ69G5FAJ"
	records := []db.ImportRecord{
		&db.CommentExport{UID: uid},
		&db.CommentExport{UID: strings.ToLower(uid)},
	}

	err := db.ValidateImportRecords(records)
	require.ErrorContains(t, err, "duplicate canonical comment UID")
	require.ErrorContains(t, db.ValidateImportReplay(records, db.ImportOptions{}), "duplicate canonical comment UID")
}

func drawImportCommentUID(ht *hegel.T) string {
	first := hegel.Draw(ht, hegel.SampledFrom([]string{"0", "1", "2", "3", "4", "5", "6", "7"}))
	tail := "A" + hegel.Draw(ht, hegel.Text().Alphabet("0123456789ABCDEFGHJKMNPQRSTVWXYZ").MinSize(24).MaxSize(24))
	return first + tail
}

func TestValidateImportRecordsRejectsCanonicalCommentUIDDuplicatesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		uid := drawImportCommentUID(ht)
		alias := strings.ToLower(uid)
		records := []db.ImportRecord{
			&db.CommentExport{UID: uid},
			&db.CommentExport{UID: alias},
		}

		require.ErrorContains(ht, db.ValidateImportRecords(records), "duplicate canonical comment UID")
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestValidateImportRecordsKeepsOpaqueCommentUIDsCaseSensitive(t *testing.T) {
	records := []db.ImportRecord{
		&db.CommentExport{UID: "legacy-comment-a"},
		&db.CommentExport{UID: "LEGACY-COMMENT-A"},
	}

	require.NoError(t, db.ValidateImportRecords(records))
}

// TestOrderImportRecordsPlacesDependenciesFirst pins the replay order the
// backends rely on: meta and project rows must land before the issue rows
// that reference them, and events after both.
func TestOrderImportRecordsPlacesDependenciesFirst(t *testing.T) {
	ordered := db.OrderImportRecords([]db.ImportRecord{
		&db.EventExport{}, &db.IssueExport{}, &db.ProjectExport{}, &db.MetaKV{},
	})
	kinds := make([]string, 0, len(ordered))
	for _, record := range ordered {
		kinds = append(kinds, record.ImportKind())
	}
	assert.Equal(t, []string{
		db.ImportKindMeta, db.ImportKindProject,
		db.ImportKindIssue, db.ImportKindEvent,
	}, kinds)
}
