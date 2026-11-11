package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestValidateImportRecordsAcceptsEveryPayloadType(t *testing.T) {
	mappingTime := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	records := []db.ImportRecord{
		&db.MetaKV{}, &db.ProjectExport{}, &db.AliasExport{},
		&db.IssueSyncBindingExport{}, &db.IssueSyncStatusExport{},
		&db.RecurrenceExport{}, &db.IssueExport{}, &db.IssueEmbeddingExport{},
		&db.CommentExport{}, &db.IssueLabelExport{}, &db.LinkExport{},
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
