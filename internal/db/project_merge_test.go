package db

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareProjectMergeRecordsPreservesEventIdentityAndPayload(t *testing.T) {
	payload := []byte(`{"link_id":3,"type":"blocks"}`)
	event := &EventExport{
		ID: 7, UID: "01H00000000000000000000003", OriginInstanceUID: "01H00000000000000000000004",
		ProjectID: 2, ProjectName: "spoke-project", Type: "issue.linked", Actor: "fixture-author",
		Payload: payload, HLCPhysicalMS: 1, ContentHash: "source-hash", CreatedAt: "2026-08-19T00:00:00.000Z",
	}
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		event,
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 20, Link: 200, Event: 300,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 2)

	assert.Equal(t, event.UID, recordPayloadAs[EventExport](t, prepared[1]).UID)
	assert.Equal(t, event.ContentHash, recordPayloadAs[EventExport](t, prepared[1]).ContentHash)
	assert.JSONEq(t, string(payload), string(recordPayloadAs[EventExport](t, prepared[1]).Payload))
	assert.JSONEq(t, string(payload), string(event.Payload), "source event payload must not be mutated")
	assert.Equal(t, "source-hash", event.ContentHash, "source event hash must not be mutated")
}

func TestPrepareProjectMergeRecordsDropsSequenceFloors(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&SequenceExport{Name: "projects", Seq: 9},
		&SequenceExport{Name: "issues", Seq: math.MaxInt64},
		&SequenceExport{Name: "events", Seq: 50},
		&SequenceExport{Name: "api_tokens", Seq: 99},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 200, Event: 100,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 1)
	assert.Equal(t, int64(4), recordPayloadAs[ProjectExport](t, prepared[0]).ID)
	assert.Equal(t, int64(9), recordPayloadAs[SequenceExport](t, records[1]).Seq, "source sequence must not be mutated")
}

func TestPrepareProjectMergeRecordsDerivesSequenceFloorsFromPurgeTombstone(t *testing.T) {
	resetCursor := int64(50)
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&PurgeLogExport{
			ID: 1, ProjectID: 2, PurgedIssueID: 3, PurgeResetAfterEventID: &resetCursor,
		},
		&SequenceExport{Name: "events", Seq: math.MaxInt64},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 100, Event: 200, PurgeLog: 300,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 4)
	assert.Equal(t, int64(103), recordPayloadAs[PurgeLogExport](t, prepared[1]).PurgedIssueID)
	require.NotNil(t, recordPayloadAs[PurgeLogExport](t, prepared[1]).PurgeResetAfterEventID)
	assert.Equal(t, int64(250), *recordPayloadAs[PurgeLogExport](t, prepared[1]).PurgeResetAfterEventID)
	require.NotNil(t, recordPayloadAs[SequenceExport](t, prepared[2]))
	assert.Equal(t, "issues", recordPayloadAs[SequenceExport](t, prepared[2]).Name)
	assert.Equal(t, int64(103), recordPayloadAs[SequenceExport](t, prepared[2]).Seq)
	require.NotNil(t, recordPayloadAs[SequenceExport](t, prepared[3]))
	assert.Equal(t, "events", recordPayloadAs[SequenceExport](t, prepared[3]).Name)
	assert.Equal(t, int64(250), recordPayloadAs[SequenceExport](t, prepared[3]).Seq)
	assert.Equal(t, int64(3), recordPayloadAs[PurgeLogExport](t, records[1]).PurgedIssueID, "source issue ID must not be mutated")
	assert.Equal(t, int64(50), *recordPayloadAs[PurgeLogExport](t, records[1]).PurgeResetAfterEventID, "source cursor must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsIDsWithoutSequenceHeadroom(t *testing.T) {
	project := &ProjectExport{
		ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project",
	}
	tests := []struct {
		name   string
		record ImportRecord
	}{
		{
			name: "issue",
			record: &IssueExport{
				ID: math.MaxInt64, UID: "01H00000000000000000000001", ProjectID: 2,
			},
		},
		{
			name: "event",
			record: &EventExport{
				ID: math.MaxInt64, UID: "01H00000000000000000000002", ProjectID: 2,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PrepareProjectMergeRecords(
				[]ImportRecord{project, tt.record},
				ProjectMergeOffsets{TargetProjectID: 4},
				nil,
			)
			require.ErrorContains(t, err, "safe ID range")
		})
	}
}

func TestPrepareProjectMergeRecordsRejectsHLCPhysicalValueWithoutHeadroom(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&EventExport{
			ID: 7, UID: "01H00000000000000000000001", ProjectID: 2,
			HLCPhysicalMS: maxProjectMergeHLCValue + 1,
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Event: 20,
	}, nil)
	require.ErrorContains(t, err, "event HLC")
	require.ErrorContains(t, err, "safe range")
}

func TestPrepareProjectMergeRecordsRejectsHLCCounterWithoutHeadroom(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&EventExport{
			ID: 7, UID: "01H00000000000000000000001", ProjectID: 2,
			HLCPhysicalMS: 1, HLCCounter: maxProjectMergeHLCValue + 1,
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Event: 20,
	}, nil)
	require.ErrorContains(t, err, "event HLC")
	require.ErrorContains(t, err, "safe range")
}

func TestPrepareProjectMergeRecordsRemapsIDs(t *testing.T) {
	issueID := int64(5)
	peerID := int64(9)
	records := []ImportRecord{
		&MetaKV{Key: "instance_uid", Value: "source"},
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&IssueExport{ID: issueID, UID: "01H00000000000000000000001", ProjectID: 2},
		&IssueExport{ID: peerID, UID: "01H00000000000000000000002", ProjectID: 2},
		&LinkExport{
			ID: 3, FromIssueID: issueID, FromIssueUID: "01H00000000000000000000001",
			ToIssueID: peerID, ToIssueUID: "01H00000000000000000000002", Type: "blocks",
		},
		&SequenceExport{Name: "issues", Seq: 99},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 20, Issue: 100, Link: 200,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 4)
	assert.Equal(t, int64(20), recordPayloadAs[ProjectExport](t, prepared[0]).ID)
	assert.Equal(t, int64(105), recordPayloadAs[IssueExport](t, prepared[1]).ID)
	assert.Equal(t, int64(20), recordPayloadAs[IssueExport](t, prepared[1]).ProjectID)
	assert.Equal(t, int64(109), recordPayloadAs[IssueExport](t, prepared[2]).ID)
	assert.Equal(t, int64(203), recordPayloadAs[LinkExport](t, prepared[3]).ID)
	assert.Equal(t, int64(105), recordPayloadAs[LinkExport](t, prepared[3]).FromIssueID)
	assert.Equal(t, int64(109), recordPayloadAs[LinkExport](t, prepared[3]).ToIssueID)

	assert.Equal(t, issueID, recordPayloadAs[IssueExport](t, records[2]).ID, "source records must not be mutated")
	assert.Equal(t, peerID, recordPayloadAs[LinkExport](t, records[4]).ToIssueID, "source link must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsRecurrenceUIDOutsideImport(t *testing.T) {
	recurrenceUID := "01H00000000000000000000002"
	occurrenceKey := "2026-08-20"
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceUID: &recurrenceUID, OccurrenceKey: &occurrenceKey,
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Recurrence: 20,
	}, nil)
	require.ErrorContains(t, err, "recurrence UID")
	require.ErrorContains(t, err, "not part of the imported project")
}

func TestPrepareProjectMergeRecordsRejectsRecurrenceIdentityMismatch(t *testing.T) {
	recurrenceID := int64(3)
	recurrenceUID := "01H00000000000000000000004"
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&RecurrenceExport{ID: 3, UID: "01H00000000000000000000002", ProjectID: 2},
		&RecurrenceExport{ID: 4, UID: recurrenceUID, ProjectID: 2},
		&IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceID: &recurrenceID, RecurrenceUID: &recurrenceUID,
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Recurrence: 20,
	}, nil)
	require.ErrorContains(t, err, "recurrence ID 3")
	require.ErrorContains(t, err, "does not match recurrence UID")
}

func TestPrepareProjectMergeRecordsRejectsRecurrenceIDOutsideImport(t *testing.T) {
	recurrenceID := int64(3)
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceID: &recurrenceID,
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Recurrence: 20,
	}, nil)
	require.ErrorContains(t, err, "recurrence ID 3")
	require.ErrorContains(t, err, "not part of the imported project")
}

func TestPrepareProjectMergeRecordsRemapsRecurrenceUIDWithinImport(t *testing.T) {
	recurrenceUID := "01H00000000000000000000002"
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&RecurrenceExport{ID: 3, UID: recurrenceUID, ProjectID: 2},
		&IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceUID: &recurrenceUID,
		},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Recurrence: 20,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, recordPayloadAs[IssueExport](t, prepared[2]).RecurrenceID)
	assert.Equal(t, int64(23), *recordPayloadAs[IssueExport](t, prepared[2]).RecurrenceID)
	assert.Nil(t, recordPayloadAs[IssueExport](t, records[2]).RecurrenceID, "source issue must not be mutated")
}

func TestPrepareProjectMergeRecordsDoesNotResolveExternalLinkPeer(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2},
		&LinkExport{
			ID: 3, FromIssueID: 5, FromIssueUID: "01H00000000000000000000001",
			ToIssueID: 9, ToIssueUID: "01H00000000000000000000002", Type: "blocks",
		},
	}
	lookupCalled := false

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Link: 20,
	}, func(string) (int64, bool, error) {
		lookupCalled = true
		return 77, true, nil
	})
	require.NoError(t, err)
	require.Len(t, prepared, 3)
	assert.Equal(t, int64(15), recordPayloadAs[LinkExport](t, prepared[2]).FromIssueID)
	assert.Zero(t, recordPayloadAs[LinkExport](t, prepared[2]).ToIssueID)
	assert.False(t, lookupCalled)
}

func TestPrepareProjectMergeRecordsClearsUnresolvedEventRelatedIssueID(t *testing.T) {
	relatedIssueID := int64(9)
	relatedIssueUID := "01H00000000000000000000002"
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&EventExport{
			ID: 7, UID: "01H00000000000000000000001", ProjectID: 2,
			RelatedIssueID: &relatedIssueID, RelatedIssueUID: &relatedIssueUID,
		},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Event: 20,
	}, func(string) (int64, bool, error) { return 0, false, nil })
	require.NoError(t, err)
	require.Len(t, prepared, 2)
	assert.Nil(t, recordPayloadAs[EventExport](t, prepared[1]).RelatedIssueID)
	require.NotNil(t, recordPayloadAs[EventExport](t, prepared[1]).RelatedIssueUID)
	assert.Equal(t, relatedIssueUID, *recordPayloadAs[EventExport](t, prepared[1]).RelatedIssueUID)
	assert.Equal(t, int64(9), *recordPayloadAs[EventExport](t, records[1]).RelatedIssueID, "source event must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsEventRelatedIssueUIDMismatch(t *testing.T) {
	relatedIssueID := int64(5)
	relatedIssueUID := "01H00000000000000000000002"
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2},
		&IssueExport{ID: 6, UID: relatedIssueUID, ProjectID: 2},
		&EventExport{
			ID: 7, UID: "01H00000000000000000000003", ProjectID: 2,
			RelatedIssueID: &relatedIssueID, RelatedIssueUID: &relatedIssueUID,
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Event: 20,
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "event related issue UID")
	assert.Contains(t, err.Error(), "does not match imported issue 5")
}

func TestPrepareProjectMergeRecordsDropsFederationState(t *testing.T) {
	projectID := int64(2)
	records := []ImportRecord{
		&ProjectExport{ID: projectID, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&FederationBindingExport{
			ProjectID: projectID, Role: "spoke", Enabled: true, PushEnabled: true,
			ReplayHorizonEventID: 40, PullCursorEventID: 50, PushCursorEventID: 60,
		},
		&FederationSyncStatusExport{ProjectID: projectID},
		&FederationQuarantineExport{
			ID: 3, ProjectID: projectID, Direction: "pull", FirstEventID: 40, LastEventID: 50,
		},
		&FederationEnrollmentExport{
			ID: 4, ProjectID: &projectID, TokenHash: "attacker-controlled", Capabilities: "pull,push",
		},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Event: 100, Quarantine: 200,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 1)
	assert.NotNil(t, recordPayloadAs[ProjectExport](t, prepared[0]))
	assert.True(t, recordPayloadAs[FederationBindingExport](t, records[1]).Enabled, "source binding must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsMultipleProjects(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&ProjectExport{ID: 3, UID: "01H00000000000000000000001", Name: "hub-project"},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{TargetProjectID: 4}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one project record")
}

func TestPrepareProjectMergeRecordsDropsWildcardFederationEnrollment(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&FederationEnrollmentExport{
			ID: 4, ProjectID: nil,
		},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Enrollment: 10,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 1)
	assert.NotNil(t, recordPayloadAs[ProjectExport](t, prepared[0]))
}

func TestPrepareProjectMergeRecordsRejectsClaimOutsideImportedProject(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2},
		&IssueClaimExport{
			ID: 6, ClaimUID: "01H00000000000000000000002", ProjectID: 2,
			IssueID: 99, IssueUID: "01H00000000000000000000003",
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Claim: 20,
	}, func(string) (int64, bool, error) { return 77, true, nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claim issue 99 is not part of the imported project")
}

func TestPrepareProjectMergeRecordsRejectsClaimIssueUIDMismatch(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2},
		&IssueClaimExport{
			ID: 6, ClaimUID: "01H00000000000000000000002", ProjectID: 2,
			IssueID: 5, IssueUID: "01H00000000000000000000003",
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Claim: 20,
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claim issue UID")
	assert.Contains(t, err.Error(), "does not match imported issue")
}

func TestPrepareProjectMergeRecordsRejectsLinkBetweenExistingProjects(t *testing.T) {
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"},
		&LinkExport{
			ID: 3, FromIssueID: 8, FromIssueUID: "01H00000000000000000000001",
			ToIssueID: 9, ToIssueUID: "01H00000000000000000000002", Type: "blocks",
		},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Link: 20,
	}, func(string) (int64, bool, error) { return 77, true, nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "link must include an issue from the imported project")
}
func TestPrepareProjectMergeRecordsPreservesExternalRootRecords(t *testing.T) {
	createdAt := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	projectUID := "01H00000000000000000000000"
	issueUID := "01H00000000000000000000001"
	bindingUID := "01H00000000000000000000002"
	records := []ImportRecord{
		&ProjectExport{ID: 2, UID: projectUID, Name: "spoke-project"},
		&IssueExport{ID: 5, UID: issueUID, ProjectID: 2},
		&ExternalFieldMappingExport{
			ConnectorInstance: "connector-one", KataField: "scheduled_on",
			ExternalFieldID: "schedule-one", ExternalFieldName: "Schedule",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-one", Active: true,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		},
		&ExternalRootBindingExport{
			UID: bindingUID, ProjectUID: projectUID, IssueUID: issueUID,
			RootMappingSource: "connector:connector-one", RootMappingExternalID: "root-one",
			ConnectorInstance: "connector-one", ExternalRootKey: "root-one",
			ExternalAccountKey: "account-one", Active: true, Enabled: true,
			ReceiveComments: true, ReceiveCommentsAfter: &createdAt,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		},
		&ExternalFieldStateExport{
			BindingUID: bindingUID, MappingConnectorInstance: "connector-one",
			MappingKataField: "scheduled_on", MappingExternalFieldID: "schedule-one",
			MappingSchemaRevision: "schema-one", MappingCreatedAt: createdAt,
			Baseline: json.RawMessage(`"2026-08-20"`), UpdatedAt: createdAt,
		},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, len(records))
	assert.Equal(t, recordPayloadAs[ExternalFieldMappingExport](t, records[2]),
		recordPayloadAs[ExternalFieldMappingExport](t, prepared[2]))
	assert.Equal(t, recordPayloadAs[ExternalRootBindingExport](t, records[3]),
		recordPayloadAs[ExternalRootBindingExport](t, prepared[3]))
	assert.Equal(t, recordPayloadAs[ExternalFieldStateExport](t, records[4]),
		recordPayloadAs[ExternalFieldStateExport](t, prepared[4]))
	assert.Equal(t, int64(2), recordPayloadAs[IssueExport](t, records[1]).ProjectID,
		"source records must not be mutated")
}

func recordPayloadAs[T any](t *testing.T, record ImportRecord) *T {
	t.Helper()
	payload, ok := any(record).(*T)
	require.True(t, ok, "record is %T, want *%T", record, *new(T))
	require.NotNil(t, payload)
	return payload
}
