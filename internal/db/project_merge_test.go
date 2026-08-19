package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	katauid "go.kenn.io/kata/internal/uid"
)

func TestPrepareProjectMergeRecordsRemapsEventLinkIDAndContentHash(t *testing.T) {
	payload := json.RawMessage(`{"link_id":3,"type":"blocks"}`)
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

	var gotPayload struct {
		LinkID int64 `json:"link_id"`
	}
	require.NoError(t, json.Unmarshal(prepared[1].Event.Payload, &gotPayload))
	assert.Equal(t, int64(203), gotPayload.LinkID)
	assert.NotEqual(t, event.UID, prepared[1].Event.UID)
	assert.True(t, katauid.Valid(prepared[1].Event.UID))
	wantHash, err := EventContentHash(EventHashInput{
		UID: prepared[1].Event.UID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: records[0].Project.UID, Type: event.Type, Actor: event.Actor,
		HLCPhysicalMS: event.HLCPhysicalMS, CreatedAt: event.CreatedAt,
		Payload: prepared[1].Event.Payload,
	})
	require.NoError(t, err)
	assert.Equal(t, wantHash, prepared[1].Event.ContentHash)
	assert.JSONEq(t, string(payload), string(event.Payload), "source event payload must not be mutated")
	assert.Equal(t, "source-hash", event.ContentHash, "source event hash must not be mutated")
}

func TestPrepareProjectMergeRecordsRemapsSequenceFloors(t *testing.T) {
	records := []ImportRecord{
		{Kind: ImportKindProject, Project: &ProjectExport{ID: 2, UID: "01H00000000000000000000000", Name: "spoke-project"}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "projects", Seq: 9}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "issues", Seq: 20}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "events", Seq: 50}},
		{Kind: ImportKindSQLiteSequence, Sequence: &SequenceExport{Name: "api_tokens", Seq: 99}},
	}

	prepared, err := PrepareProjectMergeRecords(records, ProjectMergeOffsets{
		TargetProjectID: 4, Issue: 200, Event: 100,
	}, nil)
	require.NoError(t, err)
	require.Len(t, prepared, 4)
	assert.Equal(t, "projects", prepared[1].Sequence.Name)
	assert.Equal(t, int64(11), prepared[1].Sequence.Seq)
	assert.Equal(t, "issues", prepared[2].Sequence.Name)
	assert.Equal(t, int64(220), prepared[2].Sequence.Seq)
	assert.Equal(t, "events", prepared[3].Sequence.Name)
	assert.Equal(t, int64(150), prepared[3].Sequence.Seq)
	assert.Equal(t, int64(9), records[1].Sequence.Seq, "source sequence must not be mutated")
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
	require.Len(t, prepared, 5)
	assert.Equal(t, int64(20), prepared[0].Project.ID)
	assert.Equal(t, int64(105), prepared[1].Issue.ID)
	assert.Equal(t, int64(20), prepared[1].Issue.ProjectID)
	assert.Equal(t, int64(109), prepared[2].Issue.ID)
	assert.Equal(t, int64(203), prepared[3].Link.ID)
	assert.Equal(t, int64(105), prepared[3].Link.FromIssueID)
	assert.Equal(t, int64(109), prepared[3].Link.ToIssueID)
	assert.Equal(t, "issues", prepared[4].Sequence.Name)
	assert.Equal(t, int64(199), prepared[4].Sequence.Seq)

	assert.Equal(t, issueID, records[2].Issue.ID, "source records must not be mutated")
	assert.Equal(t, peerID, records[4].Link.ToIssueID, "source link must not be mutated")
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
