package db

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const repeatedImportHubCopy = `{"uid":"issue-1","short_id":"abcd","title":"hub title","body":"hub body",
"author":"alice","owner":"bob","priority":2,"status":"open","metadata":{"team":"core","area":"sync"},
"labels":["bug","stale"],
"links":[{"type":"blocks","to_issue_uid":"issue-2","author":"alice"},{"type":"parent","to_issue_uid":"epic-1","author":"alice"}],
"comments":[
 {"comment_uid":"comment-1","author":"alice","body":"first","created_at":"2026-05-01T10:00:00.000Z"},
 {"comment_uid":"comment-hub","author":"erin","body":"only on the hub","created_at":"2026-05-01T11:00:00.000Z"}],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"2026-05-02T09:00:00.000Z"}`

const repeatedImportNewerCopy = `{"uid":"issue-1","short_id":"abcd","title":"current copy","body":"new body",
"author":"mallory","status":"closed","closed_reason":"done","closed_at":"2026-06-01T09:00:00.000Z",
"metadata":{"team":"platform"},
"labels":["bug","shipped"],
"links":[{"type":"related","to_issue_uid":"issue-3","author":"carol"},{"type":"parent","to_issue_uid":"epic-2","author":"carol"},
 {"type":"blocks","to_issue_uid":"issue-unknown","author":"carol"},{"type":"parent","to_issue_uid":"child-1","incoming":true,"author":"carol"}],
"comments":[
 {"comment_uid":"comment-1","author":"mallory","body":"first, edited","created_at":"2026-06-09T10:00:00.000Z","edited_at":"2026-05-20T10:00:00.000Z"},
 {"comment_uid":"comment-2","author":"carol","body":"second","created_at":"2026-05-21T10:00:00.000Z"}],
"created_at":"2026-05-01T09:00:00.000Z","updated_at":"2026-06-01T09:00:00.000Z"}`

func repeatedImportLookups() RepeatedSnapshotLookups {
	return RepeatedSnapshotLookups{
		IssueKnown: func(uid string) (bool, error) { return uid != "issue-unknown", nil },
		HasParent:  func(childUID string) (bool, error) { return childUID == "child-1", nil },
	}
}

// The hub turns a winning copy into its own ordinary events. Replaying them
// after the hub's history must give the incoming copy's fields, labels and
// metadata, merge its comments by UID, and only add links.
func TestRepeatedSnapshotDeltasReplayToTheIncomingCopy(t *testing.T) {
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(repeatedImportHubCopy), jsontext.Value(repeatedImportNewerCopy), repeatedImportLookups())
	require.NoError(t, err)
	require.NotEmpty(t, deltas)

	events := []FoldEvent{testEvent("issue.snapshot", 1, repeatedImportHubCopy)}
	for i, delta := range deltas {
		require.NotEqual(t, "issue.snapshot", delta.Type, "the hub stores no snapshot for a repeated import")
		event := testEvent(delta.Type, int64(10+i), string(delta.Payload))
		event.Actor = delta.Actor
		events = append(events, event)
	}
	got := FoldEvents(events)

	issue := got.Issues["issue-1"]
	assert.Equal(t, "current copy", issue.Title)
	assert.Equal(t, "new body", issue.Body)
	assert.Equal(t, "alice", issue.Author, "the issue keeps its author")
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	assert.Equal(t, "done", *issue.ClosedReason)
	require.NotNil(t, issue.ClosedAt)
	assert.Equal(t, "2026-06-01T09:00:00.000Z", *issue.ClosedAt)
	assert.Nil(t, issue.Owner)
	assert.Nil(t, issue.Priority)
	assert.Equal(t, "2026-06-01T09:00:00.000Z", issue.UpdatedAt)
	assert.ElementsMatch(t, []string{"bug", "shipped"}, presentLabels(got, "issue-1"))
	assert.JSONEq(t, `{"team":"platform"}`, string(got.IssueMetadata["issue-1"]))

	first := got.Comments["comment-1"]
	assert.Equal(t, "alice", first.Author, "a known comment keeps its author")
	assert.Equal(t, "2026-05-01T10:00:00.000Z", first.CreatedAt, "a known comment keeps its created time")
	assert.Equal(t, "first, edited", first.Body)
	assert.Equal(t, "2026-05-20T10:00:00.000Z", first.EditedAt)
	second := got.Comments["comment-2"]
	assert.Equal(t, "carol", second.Author)
	assert.Equal(t, "second", second.Body)
	assert.Equal(t, "2026-05-21T10:00:00.000Z", second.CreatedAt)
	assert.Equal(t, "only on the hub", got.Comments["comment-hub"].Body, "a comment only the hub has stays")

	links := map[FoldLinkKey]bool{}
	for _, link := range got.PresentLinks() {
		links[link.Key] = true
	}
	assert.True(t, links[FoldLinkKey{FromUID: "issue-1", ToUID: "issue-2", Type: "blocks"}], "a link only the hub has stays")
	assert.True(t, links[FoldLinkKey{FromUID: "issue-1", ToUID: "epic-1", Type: "parent"}], "the issue keeps its parent")
	assert.False(t, links[FoldLinkKey{FromUID: "issue-1", ToUID: "epic-2", Type: "parent"}], "an import does not reparent")
	assert.False(t, links[FoldLinkKey{FromUID: "child-1", ToUID: "issue-1", Type: "parent"}], "an import does not reparent another issue")
	assert.False(t, links[FoldLinkKey{FromUID: "issue-1", ToUID: "issue-unknown", Type: "blocks"}], "a link to an issue the hub lacks is not created")
	related := links[FoldLinkKey{FromUID: "issue-1", ToUID: "issue-3", Type: "related"}] ||
		links[FoldLinkKey{FromUID: "issue-3", ToUID: "issue-1", Type: "related"}]
	assert.True(t, related, "a new link is added")
}

// After the hub applies a winning copy, the same copy compares equal, so a
// second push of it does nothing.
func TestRepeatedSnapshotWinsOnlyWhenStrictlyNewer(t *testing.T) {
	hub := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		updatedAt string
		wins      bool
	}{
		"newer":                     {`"2026-06-01T09:00:01.000Z"`, true},
		"equal":                     {`"2026-06-01T09:00:00.000Z"`, false},
		"equal in another format":   {`"2026-06-01T09:00:00Z"`, false},
		"equal below a millisecond": {`"2026-06-01T09:00:00.000400Z"`, false},
		"older":                     {`"2026-05-31T09:00:00.000Z"`, false},
		"missing":                   {`null`, false},
		"unparseable":               {`"yesterday"`, false},
	} {
		t.Run(name, func(t *testing.T) {
			payload := jsontext.Value(`{"uid":"issue-1","updated_at":` + test.updatedAt + `}`)
			assert.Equal(t, test.wins, RepeatedSnapshotWins(payload, hub))
		})
	}
}

// A winning copy that differs only in updated_at still advances the time, so
// the next push of it compares equal.
func TestRepeatedSnapshotDeltasAlwaysAdvanceUpdatedAt(t *testing.T) {
	incoming := `{"uid":"issue-1","title":"same","status":"open","updated_at":"2026-06-01T09:00:00.000Z"}`
	current := `{"uid":"issue-1","title":"same","status":"open","updated_at":"2026-05-01T09:00:00.000Z"}`
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(current), jsontext.Value(incoming), repeatedImportLookups())
	require.NoError(t, err)
	require.Len(t, deltas, 1)
	assert.Equal(t, "issue.updated", deltas[0].Type)
	assert.JSONEq(t, `{"updated_at":"2026-06-01T09:00:00.000Z"}`, string(deltas[0].Payload))
}
