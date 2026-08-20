package db

import (
	"math"
	"testing"

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
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindEvent, Event: event},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 20, Link: 200, Event: 300,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 2)

	assert.Equal(t, event.UID, prepared[1].Event.UID)
	assert.Equal(t, event.ContentHash, prepared[1].Event.ContentHash)
	assert.JSONEq(t, string(payload), string(prepared[1].Event.Payload))
	assert.JSONEq(t, string(payload), string(event.Payload), "source event payload must not be mutated")
	assert.Equal(t, "source-hash", event.ContentHash, "source event hash must not be mutated")
}

func TestPrepareProjectMergeRecordsDropsSequenceFloors(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "projects", Seq: 9}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "issues", Seq: math.MaxInt64}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "events", Seq: 50}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "api_tokens", Seq: 99}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 200, Event: 100,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 1)
	assert.Equal(t, int64(4), prepared[0].Project.ID)
	assert.Equal(t, int64(9), records[1].Sequence.Seq, "source sequence must not be mutated")
}

func TestPrepareProjectMergeRecordsDerivesSequenceFloorsFromPurgeTombstone(t *testing.T) {
	resetCursor := int64(50)
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindPurgeLog, PurgeLog: &PurgeLogExport{
			ID: 1, ProjectID: 2, PurgedIssueID: 3, PurgeResetAfterEventID: &resetCursor,
		}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "events", Seq: math.MaxInt64}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 100, Event: 200, PurgeLog: 300,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 4)
	assert.Equal(t, int64(103), prepared[1].PurgeLog.PurgedIssueID)
	require.NotNil(t, prepared[1].PurgeLog.PurgeResetAfterEventID)
	assert.Equal(t, int64(250), *prepared[1].PurgeLog.PurgeResetAfterEventID)
	require.NotNil(t, prepared[2].Sequence)
	assert.Equal(t, "issues", prepared[2].Sequence.Name)
	assert.Equal(t, int64(103), prepared[2].Sequence.Seq)
	require.NotNil(t, prepared[3].Sequence)
	assert.Equal(t, "events", prepared[3].Sequence.Name)
	assert.Equal(t, int64(250), prepared[3].Sequence.Seq)
	assert.Equal(t, int64(3), records[1].PurgeLog.PurgedIssueID, "source issue ID must not be mutated")
	assert.Equal(t, int64(50), *records[1].PurgeLog.PurgeResetAfterEventID, "source cursor must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsIDsWithoutSequenceHeadroom(t *testing.T) {
	project := ImportRecord{Kind: ImportKindProject, Project: &ProjectExport{
		ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project",
	}}
	tests := []struct {
		name   string
		record ImportRecord
	}{
		{
			name: "issue",
			record: ImportRecord{Kind: ImportKindIssue, Issue: &IssueExport{
				ID: math.MaxInt64, UID: "01H00000000000000000000001", ProjectID: 2,
			}},
		},
		{
			name: "event",
			record: ImportRecord{Kind: ImportKindEvent, Event: &EventExport{
				ID: math.MaxInt64, UID: "01H00000000000000000000002", ProjectID: 2,
			}},
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
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindEvent, Event: &EventExport{
			ID: 7, UID: "01H00000000000000000000001", ProjectID: 2,
			HLCPhysicalMS: maxProjectMergeHLCValue + 1,
		}},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Event: 20,
	}, nil)
	require.ErrorContains(t, err, "event HLC")
	require.ErrorContains(t, err, "safe range")
}

func TestPrepareProjectMergeRecordsRejectsHLCCounterWithoutHeadroom(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindEvent, Event: &EventExport{
			ID: 7, UID: "01H00000000000000000000001", ProjectID: 2,
			HLCPhysicalMS: 1, HLCCounter: maxProjectMergeHLCValue + 1,
		}},
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
		{Kind: ImportKindMeta, Meta: &MetaKV{Key: "instance_uid", Value: "source"}},
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindIssue, Issue: &IssueExport{ID: issueID, UID: "01H00000000000000000000001", ProjectID: 2}},
		{Kind: ImportKindIssue, Issue: &IssueExport{ID: peerID, UID: "01H00000000000000000000002", ProjectID: 2}},
		{Kind: ImportKindLink, Link: &LinkExport{
			ID: 3, FromIssueID: issueID, FromIssueUID: "01H00000000000000000000001",
			ToIssueID: peerID, ToIssueUID: "01H00000000000000000000002", Type: "blocks",
		}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "issues", Seq: 99}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 20, Issue: 100, Link: 200,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 4)
	assert.Equal(t, int64(20), prepared[0].Project.ID)
	assert.Equal(t, int64(105), prepared[1].Issue.ID)
	assert.Equal(t, int64(20), prepared[1].Issue.ProjectID)
	assert.Equal(t, int64(109), prepared[2].Issue.ID)
	assert.Equal(t, int64(203), prepared[3].Link.ID)
	assert.Equal(t, int64(105), prepared[3].Link.FromIssueID)
	assert.Equal(t, int64(109), prepared[3].Link.ToIssueID)

	assert.Equal(t, issueID, records[2].Issue.ID, "source records must not be mutated")
	assert.Equal(t, peerID, records[4].Link.ToIssueID, "source link must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsRecurrenceUIDOutsideImport(t *testing.T) {
	recurrenceUID := "01H00000000000000000000002"
	occurrenceKey := "2026-08-20"
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindIssue, Issue: &IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceUID: &recurrenceUID, OccurrenceKey: &occurrenceKey,
		}},
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
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindRecurrence, Recurrence: &RecurrenceExport{ID: 3, UID: "01H00000000000000000000002", ProjectID: 2}},
		{Kind: ImportKindRecurrence, Recurrence: &RecurrenceExport{ID: 4, UID: recurrenceUID, ProjectID: 2}},
		{Kind: ImportKindIssue, Issue: &IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceID: &recurrenceID, RecurrenceUID: &recurrenceUID,
		}},
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
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindIssue, Issue: &IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceID: &recurrenceID,
		}},
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
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindRecurrence, Recurrence: &RecurrenceExport{ID: 3, UID: recurrenceUID, ProjectID: 2}},
		{Kind: ImportKindIssue, Issue: &IssueExport{
			ID: 5, UID: "01H00000000000000000000001", ProjectID: 2,
			RecurrenceUID: &recurrenceUID,
		}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Recurrence: 20,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, prepared[2].Issue.RecurrenceID)
	assert.Equal(t, int64(23), *prepared[2].Issue.RecurrenceID)
	assert.Nil(t, records[2].Issue.RecurrenceID, "source issue must not be mutated")
}

func TestPrepareProjectMergeRecordsDoesNotResolveExternalLinkPeer(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindIssue, Issue: &IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2}},
		{Kind: ImportKindLink, Link: &LinkExport{
			ID: 3, FromIssueID: 5, FromIssueUID: "01H00000000000000000000001",
			ToIssueID: 9, ToIssueUID: "01H00000000000000000000002", Type: "blocks",
		}},
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
	assert.Equal(t, int64(15), prepared[2].Link.FromIssueID)
	assert.Zero(t, prepared[2].Link.ToIssueID)
	assert.False(t, lookupCalled)
}

func TestPrepareProjectMergeRecordsClearsUnresolvedEventRelatedIssueID(t *testing.T) {
	relatedIssueID := int64(9)
	relatedIssueUID := "01H00000000000000000000002"
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindEvent, Event: &EventExport{
			ID: 7, UID: "01H00000000000000000000001", ProjectID: 2,
			RelatedIssueID: &relatedIssueID, RelatedIssueUID: &relatedIssueUID,
		}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Event: 20,
	}, func(string) (int64, bool, error) { return 0, false, nil })
	require.NoError(t, err)
	require.Len(t, prepared, 2)
	assert.Nil(t, prepared[1].Event.RelatedIssueID)
	require.NotNil(t, prepared[1].Event.RelatedIssueUID)
	assert.Equal(t, relatedIssueUID, *prepared[1].Event.RelatedIssueUID)
	assert.Equal(t, int64(9), *records[1].Event.RelatedIssueID, "source event must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsEventRelatedIssueUIDMismatch(t *testing.T) {
	relatedIssueID := int64(5)
	relatedIssueUID := "01H00000000000000000000002"
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindIssue, Issue: &IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2}},
		{Kind: ImportKindIssue, Issue: &IssueExport{ID: 6, UID: relatedIssueUID, ProjectID: 2}},
		{Kind: ImportKindEvent, Event: &EventExport{
			ID: 7, UID: "01H00000000000000000000003", ProjectID: 2,
			RelatedIssueID: &relatedIssueID, RelatedIssueUID: &relatedIssueUID,
		}},
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
		{Kind: ImportKindProject, Project: &ProjectExport{ID: projectID, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindFederationBinding, FederationBinding: &FederationBindingExport{
			ProjectID: projectID, Role: "spoke", Enabled: true, PushEnabled: true,
			ReplayHorizonEventID: 40, PullCursorEventID: 50, PushCursorEventID: 60,
		}},
		{Kind: ImportKindFederationSyncStatus, FederationSyncStatus: &FederationSyncStatusExport{ProjectID: projectID}},
		{Kind: ImportKindFederationQuarantine, FederationQuarantine: &FederationQuarantineExport{
			ID: 3, ProjectID: projectID, Direction: "pull", FirstEventID: 40, LastEventID: 50,
		}},
		{Kind: ImportKindFederationEnrollment, FederationEnrollment: &FederationEnrollmentExport{
			ID: 4, ProjectID: &projectID, TokenHash: "attacker-controlled", Capabilities: "pull,push",
		}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Event: 100, Quarantine: 200,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 1)
	assert.NotNil(t, prepared[0].Project)
	for _, record := range prepared {
		assert.Nil(t, record.FederationBinding)
		assert.Nil(t, record.FederationSyncStatus)
		assert.Nil(t, record.FederationQuarantine)
		assert.Nil(t, record.FederationEnrollment)
	}
	assert.True(t, records[1].FederationBinding.Enabled, "source binding must not be mutated")
}

func TestPrepareProjectMergeRecordsRejectsMultipleProjects(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 3, UID: "01H00000000000000000000001", Name: "hub-project"}},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{TargetProjectID: 4}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one project record")
}

func TestPrepareProjectMergeRecordsDropsWildcardFederationEnrollment(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindFederationEnrollment, FederationEnrollment: &FederationEnrollmentExport{
			ID: 4, ProjectID: nil,
		}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Enrollment: 10,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 1)
	assert.NotNil(t, prepared[0].Project)
}

func TestPrepareProjectMergeRecordsRejectsClaimOutsideImportedProject(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindIssue, Issue: &IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2}},
		{Kind: ImportKindIssueClaim, IssueClaim: &IssueClaimExport{
			ID: 6, ClaimUID: "01H00000000000000000000002", ProjectID: 2,
			IssueID: 99, IssueUID: "01H00000000000000000000003",
		}},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 10, Claim: 20,
	}, func(string) (int64, bool, error) { return 77, true, nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claim issue 99 is not part of the imported project")
}

func TestPrepareProjectMergeRecordsRejectsClaimIssueUIDMismatch(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindIssue, Issue: &IssueExport{ID: 5, UID: "01H00000000000000000000001", ProjectID: 2}},
		{Kind: ImportKindIssueClaim, IssueClaim: &IssueClaimExport{
			ID: 6, ClaimUID: "01H00000000000000000000002", ProjectID: 2,
			IssueID: 5, IssueUID: "01H00000000000000000000003",
		}},
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
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindLink, Link: &LinkExport{
			ID: 3, FromIssueID: 8, FromIssueUID: "01H00000000000000000000001",
			ToIssueID: 9, ToIssueUID: "01H00000000000000000000002", Type: "blocks",
		}},
	}

	_, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Link: 20,
	}, func(string) (int64, bool, error) { return 77, true, nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "link must include an issue from the imported project")
}
