package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const repeatedSnapshotBaseline = `{"uid":"issue-1","short_id":"abcd","title":"first import","body":"old body",
"author":"alice","owner":"bob","priority":2,"status":"open","metadata":{"team":"core"},
"labels":["bug","stale"],"links":[{"type":"blocks","to_issue_uid":"issue-2","author":"alice"}],
"comments":[{"comment_uid":"comment-1","author":"alice","body":"first","created_at":"2026-05-01T10:00:00.000Z"}],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"2026-05-02T09:00:00.000Z"}`

func repeatedSnapshotEvent(n int64, origin, payload string) FoldEvent {
	event := testEvent("issue.snapshot", n, payload)
	event.OriginInstanceUID = origin
	return event
}

func presentLabels(p FoldProjection, issueUID string) []string {
	var labels []string
	for key, state := range p.Labels {
		if key.IssueUID == issueUID && state.Present {
			labels = append(labels, key.Label)
		}
	}
	return labels
}

func TestFoldRepeatedSnapshotWithNewerStateReplacesIssue(t *testing.T) {
	events := []FoldEvent{
		repeatedSnapshotEvent(1, "spoke-a", repeatedSnapshotBaseline),
		testEvent("issue.labeled", 2, `{"label":"triage","updated_at":"2026-05-03T09:00:00.000Z"}`),
		repeatedSnapshotEvent(10, "spoke-b", `{"uid":"issue-1","short_id":"abcd","title":"current copy","body":"new body",
"author":"alice","status":"closed","closed_reason":"done","closed_at":"2026-06-01T09:00:00.000Z",
"labels":["bug","shipped"],
"comments":[
 {"comment_uid":"comment-1","author":"mallory","body":"first, edited","created_at":"2026-06-09T10:00:00.000Z","edited_at":"2026-05-20T10:00:00.000Z"},
 {"comment_uid":"comment-2","author":"carol","body":"second","created_at":"2026-05-21T10:00:00.000Z"}],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"2026-06-01T09:00:00.000Z"}`),
	}

	got := FoldEvents(events)

	issue := got.Issues["issue-1"]
	assert.Equal(t, "current copy", issue.Title)
	assert.Equal(t, "new body", issue.Body)
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	assert.Equal(t, "done", *issue.ClosedReason)
	assert.Nil(t, issue.Owner)
	assert.Nil(t, issue.Priority)
	assert.Equal(t, "2026-06-01T09:00:00.000Z", issue.UpdatedAt)
	assert.ElementsMatch(t, []string{"bug", "shipped"}, presentLabels(got, "issue-1"))
	assert.Len(t, got.PresentLinks(), 1, "links merge additively and are never removed by a snapshot")
	assert.Empty(t, got.IssueMetadata["issue-1"], "the newer snapshot carries no metadata")

	first := got.Comments["comment-1"]
	assert.Equal(t, "alice", first.Author, "a known comment keeps its original author")
	assert.Equal(t, "2026-05-01T10:00:00.000Z", first.CreatedAt, "a known comment keeps its original created time")
	assert.Equal(t, "first, edited", first.Body)
	assert.Equal(t, "2026-05-20T10:00:00.000Z", first.EditedAt)
	second := got.Comments["comment-2"]
	assert.Equal(t, "carol", second.Author)
	assert.Equal(t, "2026-05-21T10:00:00.000Z", second.CreatedAt)
}

func TestFoldRepeatedSnapshotWithOlderStateKeepsIssue(t *testing.T) {
	events := []FoldEvent{
		repeatedSnapshotEvent(1, "spoke-a", repeatedSnapshotBaseline),
		repeatedSnapshotEvent(10, "spoke-b", `{"uid":"issue-1","short_id":"abcd","title":"stale copy","body":"stale body",
"author":"alice","status":"closed","labels":["ancient"],
"comments":[{"comment_uid":"comment-0","author":"dave","body":"only in the stale copy","created_at":"2026-04-30T10:00:00.000Z"}],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"2026-05-01T12:00:00.000Z"}`),
	}

	got := FoldEvents(events)

	issue := got.Issues["issue-1"]
	assert.Equal(t, "first import", issue.Title)
	assert.Equal(t, "old body", issue.Body)
	assert.Equal(t, "open", issue.Status)
	require.NotNil(t, issue.Owner)
	assert.Equal(t, "bob", *issue.Owner)
	assert.Equal(t, "2026-05-02T09:00:00.000Z", issue.UpdatedAt)
	assert.ElementsMatch(t, []string{"bug", "stale"}, presentLabels(got, "issue-1"))
	assert.Len(t, got.PresentLinks(), 1)
	assert.JSONEq(t, `{"team":"core"}`, string(got.IssueMetadata["issue-1"]))
	assert.Equal(t, "dave", got.Comments["comment-0"].Author, "a comment the hub lacks is never dropped")
	assert.Contains(t, got.Comments, "comment-1")
}

func TestFoldRepeatedIdenticalSnapshotChangesNothing(t *testing.T) {
	once := FoldEvents([]FoldEvent{repeatedSnapshotEvent(1, "spoke-a", repeatedSnapshotBaseline)})
	twice := FoldEvents([]FoldEvent{
		repeatedSnapshotEvent(1, "spoke-a", repeatedSnapshotBaseline),
		repeatedSnapshotEvent(10, "spoke-b", repeatedSnapshotBaseline),
	})

	assert.Equal(t, once.Issues["issue-1"].Title, twice.Issues["issue-1"].Title)
	assert.Equal(t, once.Issues["issue-1"].UpdatedAt, twice.Issues["issue-1"].UpdatedAt)
	assert.Equal(t, once.Issues["issue-1"].Owner, twice.Issues["issue-1"].Owner)
	assert.ElementsMatch(t, presentLabels(once, "issue-1"), presentLabels(twice, "issue-1"))
	assert.Len(t, twice.PresentLinks(), len(once.PresentLinks()))
	assert.Equal(t, jsontext.Value(once.IssueMetadata["issue-1"]), twice.IssueMetadata["issue-1"])
	assert.Equal(t, once.Comments["comment-1"].Body, twice.Comments["comment-1"].Body)
	assert.Empty(t, twice.Warnings)
}

// Events after the repeated snapshot still apply on top of it.
func TestFoldEditAfterRepeatedSnapshotStillApplies(t *testing.T) {
	events := []FoldEvent{
		repeatedSnapshotEvent(1, "spoke-a", repeatedSnapshotBaseline),
		repeatedSnapshotEvent(10, "spoke-b", `{"uid":"issue-1","short_id":"abcd","title":"current copy","body":"new body",
"author":"alice","status":"open","labels":["bug"],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"2026-06-01T09:00:00.000Z"}`),
		testEvent("issue.labeled", 11, `{"label":"after","updated_at":"2026-06-02T09:00:00.000Z"}`),
	}

	got := FoldEvents(events)

	assert.Equal(t, "current copy", got.Issues["issue-1"].Title)
	assert.ElementsMatch(t, []string{"bug", "after"}, presentLabels(got, "issue-1"))
}

// A newer snapshot may carry a later edit time for a comment whose text is
// unchanged. The fold must record it, or a later import with an intermediate
// edit would overwrite the restored text.
func TestFoldRepeatedSnapshotRecordsCommentEditTimeForUnchangedText(t *testing.T) {
	snapshot := func(n int64, origin, updatedAt, body, editedAt string) FoldEvent {
		return repeatedSnapshotEvent(n, origin, `{"uid":"issue-1","title":"task","status":"open",
"comments":[{"comment_uid":"comment-1","author":"alice","body":"`+body+`","created_at":"2026-05-01T10:00:00.000Z","edited_at":"`+editedAt+`"}],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"`+updatedAt+`"}`)
	}
	events := []FoldEvent{
		snapshot(1, "spoke-a", "2026-05-02T09:00:00.000Z", "final text", "2026-05-01T11:00:00.000Z"),
		snapshot(10, "spoke-b", "2026-06-01T09:00:00.000Z", "final text", "2026-05-20T10:00:00.000Z"),
		snapshot(20, "spoke-c", "2026-07-01T09:00:00.000Z", "intermediate text", "2026-05-10T10:00:00.000Z"),
	}

	got := FoldEvents(events)

	comment := got.Comments["comment-1"]
	assert.Equal(t, "final text", comment.Body)
	assert.Equal(t, "2026-05-20T10:00:00.000Z", comment.EditedAt)
	assert.Equal(t, "alice", comment.Author)
}

// A repeated snapshot adds links but never replaces a child's parent, whether
// the copy is older or newer. Reparenting goes through an ordinary link change.
func TestFoldRepeatedSnapshotKeepsEstablishedParent(t *testing.T) {
	child := func(n int64, origin, updatedAt, parent string) FoldEvent {
		return repeatedSnapshotEvent(n, origin, `{"uid":"issue-1","title":"child","status":"open",
"links":[{"type":"parent","to_issue_uid":"`+parent+`","author":"alice"},{"type":"blocks","to_issue_uid":"`+parent+`","author":"alice"}],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"`+updatedAt+`"}`)
	}
	for name, updatedAt := range map[string]string{"older copy": "2026-05-01T12:00:00.000Z", "newer copy": "2026-06-01T09:00:00.000Z"} {
		t.Run(name, func(t *testing.T) {
			got := FoldEvents([]FoldEvent{
				child(1, "spoke-a", "2026-05-02T09:00:00.000Z", "first-parent"),
				child(10, "spoke-b", updatedAt, "other-parent"),
			})
			var parents, blocks []string
			for _, edge := range got.PresentLinks() {
				switch edge.Key.Type {
				case "parent":
					parents = append(parents, edge.Key.ToUID)
				case "blocks":
					blocks = append(blocks, edge.Key.ToUID)
				}
			}
			assert.Equal(t, []string{"first-parent"}, parents)
			assert.ElementsMatch(t, []string{"first-parent", "other-parent"}, blocks, "a link that does not conflict is added")
		})
	}
	reparented := FoldEvents([]FoldEvent{
		child(1, "spoke-a", "2026-05-02T09:00:00.000Z", "first-parent"),
		testEvent("issue.links_changed", 5, `{"parent_set_uid":"other-parent"}`),
	})
	for _, edge := range reparented.PresentLinks() {
		if edge.Key.Type == "parent" {
			assert.Equal(t, "other-parent", edge.Key.ToUID, "an explicit link change still reparents")
		}
	}
}
