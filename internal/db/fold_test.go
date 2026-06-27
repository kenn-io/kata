package db

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testEvent(eventType string, n int64, payload string) FoldEvent {
	return FoldEvent{
		UID:               fmt.Sprintf("event-%03d", n),
		OriginInstanceUID: "spoke-a",
		ProjectUID:        "project-1",
		IssueUID:          "issue-1",
		Type:              eventType,
		Actor:             "agent",
		HLCPhysicalMS:     n,
		HLCCounter:        0,
		CreatedAt:         "2026-05-23T12:00:00.000Z",
		Payload:           json.RawMessage(payload),
	}
}

func TestFold_OrderIndependentForDifferentIssueFields(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.updated", 3, `{"body":"new body"}`),
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"old","body":"old body","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.updated", 2, `{"title":"new title","old_title":"old"}`),
	}

	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Equal(t, "new title", issue.Title)
	assert.Equal(t, "new body", issue.Body)
	assert.Equal(t, "open", issue.Status)
	assert.Equal(t, "abcd", issue.ShortID)
}

func TestFoldIssueUpdatedAppliesReplayCompleteScalars(t *testing.T) {
	priority := int64(3)
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"old","body":"old body","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","updated_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.updated", 2, `{"title":"new","body":"new body","priority":3,"status":"closed","closed_reason":"done","closed_at":"2026-05-23T12:00:05.000Z","updated_at":"2026-05-23T12:00:06.000Z"}`),
	}

	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Equal(t, "new", issue.Title)
	assert.Equal(t, "new body", issue.Body)
	assert.Equal(t, &priority, issue.Priority)
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	assert.Equal(t, "done", *issue.ClosedReason)
	require.NotNil(t, issue.ClosedAt)
	assert.Equal(t, "2026-05-23T12:00:05.000Z", *issue.ClosedAt)
	assert.Equal(t, "2026-05-23T12:00:06.000Z", issue.UpdatedAt)
}

func TestFoldIssueUpdatedHealsCreatedAtEarlier(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-10T10:00:00.000Z","updated_at":"2026-05-15T10:00:00.000Z"}`),
		testEvent("issue.updated", 2, `{"created_at":"2026-05-01T10:00:00.000Z","updated_at":"2026-05-15T10:00:00.000Z"}`),
	}

	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Equal(t, "2026-05-01T10:00:00.000Z", issue.CreatedAt,
		"issue.updated must heal created_at to the earlier corrected timestamp")
}

func TestFoldIssueUpdatedDoesNotPushCreatedAtLater(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-01T10:00:00.000Z","updated_at":"2026-05-15T10:00:00.000Z"}`),
		testEvent("issue.updated", 2, `{"created_at":"2026-05-10T10:00:00.000Z","updated_at":"2026-05-15T10:00:00.000Z"}`),
	}

	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Equal(t, "2026-05-01T10:00:00.000Z", issue.CreatedAt,
		"issue.updated must not push created_at later than the recorded creation time")
}

func TestFold_LifecycleOwnerPriorityLabelsLinksAndComments(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.assigned", 2, `{"owner":"alice"}`),
		testEvent("issue.priority_set", 3, `{"priority":1}`),
		testEvent("issue.labeled", 4, `{"issue_uid":"issue-1","label":"bug"}`),
		testEvent("issue.linked", 5, `{"type":"related","from_uid":"issue-2","to_uid":"issue-1"}`),
		testEvent("issue.commented", 6, `{"comment_uid":"comment-1","author":"agent","body":"note","created_at":"2026-05-23T12:00:06.000Z"}`),
		testEvent("issue.closed", 7, `{"reason":"done","closed_at":"2026-05-23T12:00:07.000Z"}`),
		testEvent("issue.reopened", 8, `{"reopened_at":"2026-05-23T12:00:08.000Z"}`),
		testEvent("issue.priority_cleared", 9, `{"old_priority":1}`),
		testEvent("issue.unassigned", 10, `{"owner":null}`),
	}

	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Nil(t, issue.Owner)
	assert.Nil(t, issue.Priority)
	assert.Equal(t, "open", issue.Status)
	assert.Nil(t, issue.ClosedReason)
	assert.Nil(t, issue.ClosedAt)
	assert.True(t, got.Labels[FoldLabelKey{IssueUID: "issue-1", Label: "bug"}].Present)
	assert.True(t, got.Links[FoldLinkKey{FromUID: "issue-1", ToUID: "issue-2", Type: "related"}].Present)
	comment := got.Comments["comment-1"]
	assert.Equal(t, "issue-1", comment.IssueUID)
	assert.Equal(t, "note", comment.Body)
}

func TestFoldReopenRestoreUsePayloadIssueUID(t *testing.T) {
	// Ingest validation accepts an issue event whose uid lives only in the
	// payload (empty envelope issue_uid). reopened/restored must apply such an
	// event like every other issue handler instead of dropping it.
	payloadOnlyUID := func(eventType string, n int64, payload string) FoldEvent {
		e := testEvent(eventType, n, payload)
		e.IssueUID = ""
		return e
	}
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","updated_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.closed", 2, `{"reason":"done","closed_at":"2026-05-23T12:00:02.000Z"}`),
		payloadOnlyUID("issue.reopened", 3, `{"issue_uid":"issue-1","reopened_at":"2026-05-23T12:00:03.000Z"}`),
		testEvent("issue.soft_deleted", 4, `{"deleted_at":"2026-05-23T12:00:04.000Z"}`),
		payloadOnlyUID("issue.restored", 5, `{"issue_uid":"issue-1","restored_at":"2026-05-23T12:00:05.000Z"}`),
	}

	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Equal(t, "open", issue.Status)
	assert.Nil(t, issue.ClosedAt)
	assert.Nil(t, issue.DeletedAt)
}

func TestFoldClaimEventsAreAuditOnly(t *testing.T) {
	baselineEvents := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"body","author":"agent","status":"open","metadata":{"area":"db"},"created_at":"2026-05-23T12:00:00.000Z","updated_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.assigned", 2, `{"owner":"alice"}`),
		testEvent("issue.priority_set", 3, `{"priority":1}`),
		testEvent("issue.labeled", 4, `{"issue_uid":"issue-1","label":"bug"}`),
		testEvent("issue.linked", 5, `{"type":"related","from_uid":"issue-1","to_uid":"issue-2"}`),
		testEvent("issue.commented", 6, `{"comment_uid":"comment-1","author":"agent","body":"note","created_at":"2026-05-23T12:00:06.000Z"}`),
		testEvent("issue.metadata_updated", 7, `{"diff":{"area":{"from":"db","to":"federation"}}}`),
	}
	claimEvents := []FoldEvent{
		testEvent("claim.acquired", 8, `{"claim_uid":"claim-1","issue_uid":"issue-1","holder":"spoke-a","purpose":"edit"}`),
		testEvent("claim.released", 9, `{"claim_uid":"claim-1","issue_uid":"issue-1","reason":"done"}`),
		testEvent("claim.expired", 10, `{"claim_uid":"claim-1","issue_uid":"issue-1","reason":"expired"}`),
		testEvent("claim.force_released", 11, `{"claim_uid":"claim-1","issue_uid":"issue-1","reason":"operator"}`),
		testEvent("claim.violated", 12, `{"claim_uid":"claim-1","issue_uid":"issue-1","reason":"conflict"}`),
	}

	want := FoldEvents(baselineEvents)
	got := FoldEvents(append(baselineEvents, claimEvents...))

	assert.Equal(t, want, got)
}

func TestFoldLabelAndLinkUsePayloadUpdatedAtWhenPresent(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","updated_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.labeled", 2, `{"issue_uid":"issue-1","label":"bug","updated_at":"2026-05-23T12:00:01.000Z"}`),
		testEvent("issue.linked", 3, `{"issue_uid":"issue-1","type":"related","from_uid":"issue-1","to_uid":"issue-2","updated_at":"2026-05-23T12:00:02.000Z"}`),
	}
	events[1].CreatedAt = "2026-05-23T13:00:00.000Z"
	events[2].CreatedAt = "2026-05-23T14:00:00.000Z"

	got := FoldEvents(events)

	assert.Equal(t, "2026-05-23T12:00:02.000Z", got.Issues["issue-1"].UpdatedAt)
}

func TestFoldLinksChangedUsesPayloadIssueUID(t *testing.T) {
	changed := testEvent("issue.links_changed", 2,
		`{"issue_uid":"issue-1","blocks_added_uids":["issue-2"],"updated_at":"2026-05-23T12:00:02.000Z"}`)
	changed.IssueUID = ""
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","updated_at":"2026-05-23T12:00:00.000Z"}`),
		changed,
	}

	got := FoldEvents(events)

	assert.True(t, got.Links[FoldLinkKey{FromUID: "issue-1", ToUID: "issue-2", Type: "blocks"}].Present)
	assert.Equal(t, "2026-05-23T12:00:02.000Z", got.Issues["issue-1"].UpdatedAt)
}

func TestFoldParentLinksAreSingleChildLWWRegister(t *testing.T) {
	t.Run("issue.linked parent events", func(t *testing.T) {
		events := []FoldEvent{
			testEvent("issue.linked", 3, `{"type":"parent","from_uid":"issue-1","to_uid":"parent-b"}`),
			testEvent("issue.linked", 2, `{"type":"parent","from_uid":"issue-1","to_uid":"parent-a"}`),
		}

		got := FoldEvents(events)

		assert.False(t, got.Links[FoldLinkKey{FromUID: "issue-1", ToUID: "parent-a", Type: "parent"}].Present)
		assert.True(t, got.Links[FoldLinkKey{FromUID: "issue-1", ToUID: "parent-b", Type: "parent"}].Present)
	})

	t.Run("issue.links_changed parent_set events", func(t *testing.T) {
		events := []FoldEvent{
			testEvent("issue.links_changed", 2, `{"issue_uid":"issue-1","parent_set_uid":"parent-a"}`),
			testEvent("issue.links_changed", 3, `{"issue_uid":"issue-1","parent_set_uid":"parent-b"}`),
		}

		got := FoldEvents(events)

		assert.False(t, got.Links[FoldLinkKey{FromUID: "issue-1", ToUID: "parent-a", Type: "parent"}].Present)
		assert.True(t, got.Links[FoldLinkKey{FromUID: "issue-1", ToUID: "parent-b", Type: "parent"}].Present)
	})
}

func TestFoldIssueSnapshotEstablishesIssueStateAndBaselineComments(t *testing.T) {
	priority := int64(2)
	owner := "alice"
	events := []FoldEvent{
		testEvent("issue.snapshot", 10, `{
			"uid":"issue-1",
			"short_id":"abcd",
			"title":"snapshotted",
			"body":"body",
			"author":"alice",
			"owner":"alice",
			"priority":2,
			"status":"closed",
			"closed_reason":"done",
			"closed_at":"2026-05-23T12:00:10.000Z",
			"deleted_at":"2026-05-23T12:00:11.000Z",
			"metadata":{"area":"db"},
			"labels":["area:db"],
			"links":[{"type":"related","to_issue_uid":"issue-2"}],
			"comments":[{"comment_uid":"comment-1","author":"bob","body":"baseline note","created_at":"2026-05-23T12:00:12.000Z"}],
			"created_at":"2026-05-23T12:00:00.000Z"
		}`),
	}

	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Equal(t, "abcd", issue.ShortID)
	assert.Equal(t, "snapshotted", issue.Title)
	assert.Equal(t, "body", issue.Body)
	assert.Equal(t, "alice", issue.Author)
	assert.Equal(t, &owner, issue.Owner)
	assert.Equal(t, &priority, issue.Priority)
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	assert.Equal(t, "done", *issue.ClosedReason)
	require.NotNil(t, issue.DeletedAt)
	assert.True(t, got.Labels[FoldLabelKey{IssueUID: "issue-1", Label: "area:db"}].Present)
	assert.True(t, got.Links[FoldLinkKey{FromUID: "issue-1", ToUID: "issue-2", Type: "related"}].Present)
	comment := got.Comments["comment-1"]
	assert.Equal(t, "issue-1", comment.IssueUID)
	assert.Equal(t, "bob", comment.Author)
	assert.Equal(t, "baseline note", comment.Body)
	require.JSONEq(t, `{"area":"db"}`, string(got.IssueMetadata["issue-1"]))
}

func TestFold_MetadataDiffsTreatTopLevelNullAsDeletion(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{"area":"api"},"created_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.metadata_updated", 2, `{"diff":{"area":{"from":"api","to":"ui"},"gone":{"from":"x","to":null}}}`),
	}

	got := FoldEvents(events)
	require.JSONEq(t, `{"area":"ui"}`, string(got.IssueMetadata["issue-1"]))
}

func TestFold_MetadataDiffsMergeNestedObjectPaths(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{"settings":{"theme":"dark","size":1}},"created_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.metadata_updated", 2, `{"diff":{"settings":{"from":{"size":1},"to":{"size":2}}}}`),
	}

	got := FoldEvents(events)
	require.JSONEq(t, `{"settings":{"theme":"dark","size":2}}`, string(got.IssueMetadata["issue-1"]))
}

func TestFold_CommentDuplicateKeepsFirstAndWarns(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.commented", 2, `{"comment_uid":"comment-1","author":"agent","body":"first","created_at":"2026-05-23T12:00:01.000Z"}`),
		testEvent("issue.commented", 3, `{"comment_uid":"comment-1","author":"agent","body":"second","created_at":"2026-05-23T12:00:02.000Z"}`),
	}

	got := FoldEvents(events)
	assert.Equal(t, "first", got.Comments["comment-1"].Body)
	require.Len(t, got.Warnings, 1)
}

func TestFold_CommentEditOverwritesBodyOnly(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","short_id":"abcd","title":"t","body":"","author":"agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`),
		testEvent("issue.commented", 2, `{"comment_uid":"comment-1","author":"agent","body":"token=leaked","created_at":"2026-05-23T12:00:01.000Z"}`),
		testEvent("issue.comment_edited", 3, `{"comment_uid":"comment-1","body":"[redacted]","edited_at":"2026-05-23T12:00:02.000Z"}`),
	}

	got := FoldEvents(events)
	comment := got.Comments["comment-1"]
	assert.Equal(t, "agent", comment.Author)
	assert.Equal(t, "[redacted]", comment.Body)
	assert.Equal(t, "2026-05-23T12:00:01.000Z", comment.CreatedAt)
	assert.Empty(t, got.Warnings)
}
