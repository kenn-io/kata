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

func repeatedImportLookups() *RepeatedSnapshotLookups {
	return &RepeatedSnapshotLookups{
		IssueKnown: func(uid string) (bool, error) { return uid != "issue-unknown", nil },
		ParentOf: func(childUID string) (string, error) {
			if childUID == "child-1" {
				return "other-parent", nil
			}
			return "", nil
		},
	}
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
	assert.Equal(t, RepeatedImportEventType, deltas[0].Type)
	got := FoldEvents([]FoldEvent{testEvent("issue.snapshot", 1, current), testEvent(deltas[0].Type, 10, string(deltas[0].Payload))})
	assert.Equal(t, "2026-06-01T09:00:00.000Z", got.Issues["issue-1"].UpdatedAt)
}

// An edit older than the winning copy can reach the hub after the import. It
// replays before the hub's events for the copy, so those events must restate
// the whole copy: the values it shares with the hub, and its complete label
// set and metadata, which a late edit may have added to.
func TestRepeatedSnapshotDeltasOutlastADelayedOlderEdit(t *testing.T) {
	current := `{"uid":"issue-1","title":"agreed title","body":"agreed body","owner":"bob","priority":2,"status":"open",
"metadata":{"team":{"name":"core"},"cron.occurrence":{"n":3}},"labels":["bug"],"updated_at":"2026-05-01T09:00:00.000Z"}`
	incoming := `{"uid":"issue-1","title":"agreed title","body":"agreed body","owner":"bob","priority":2,"status":"open",
"metadata":{"team":{"name":"core"}},"labels":["bug"],"updated_at":"2026-06-01T09:00:00.000Z"}`
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(current), jsontext.Value(incoming), repeatedImportLookups())
	require.NoError(t, err)

	events := []FoldEvent{
		testEvent("issue.snapshot", 1, current),
		testEvent("issue.updated", 5, `{"title":"delayed title","body":"delayed body","owner":null,"priority":null,"status":"closed","closed_reason":"done","updated_at":"2026-05-10T09:00:00.000Z"}`),
		testEvent("issue.unlabeled", 6, `{"label":"bug"}`),
		testEvent("issue.labeled", 7, `{"label":"stray"}`),
		testEvent("issue.metadata_updated", 8, `{"diff":{"team":{"from":{"name":"core"},"to":{"name":"core","lead":"zed"}},"stray":{"from":null,"to":"x"}}}`),
	}
	for i, delta := range deltas {
		events = append(events, testEvent(delta.Type, int64(10+i), string(delta.Payload)))
	}
	events = append(events, testEvent("issue.labeled", 30, `{"label":"later"}`))
	got := FoldEvents(events)
	issue := got.Issues["issue-1"]
	assert.Equal(t, "agreed title", issue.Title)
	assert.Equal(t, "agreed body", issue.Body)
	require.NotNil(t, issue.Owner)
	assert.Equal(t, "bob", *issue.Owner)
	require.NotNil(t, issue.Priority)
	assert.Equal(t, int64(2), *issue.Priority)
	assert.Equal(t, "open", issue.Status)
	assert.Nil(t, issue.ClosedReason)
	assert.Nil(t, issue.ClosedAt)
	assert.ElementsMatch(t, []string{"bug", "later"}, presentLabels(got, "issue-1"), "a label added after the copy stays")
	assert.JSONEq(t, `{"team":{"name":"core"},"cron.occurrence":{"n":3}}`, string(got.IssueMetadata["issue-1"]),
		"the copy's metadata replaces the rest, and the hub's recurrence keys stay")
}

// A comment the hub lacks can arrive again later as its original creation and
// earlier edits from another home. The copy's newer text must still win.
func TestRepeatedSnapshotDeltasKeepAnImportedCommentEdit(t *testing.T) {
	current := `{"uid":"issue-1","title":"t","status":"open","updated_at":"2026-05-01T09:00:00.000Z"}`
	incoming := `{"uid":"issue-1","title":"t","status":"open","comments":[
 {"comment_uid":"comment-2","author":"carol","body":"latest text","created_at":"2026-05-05T10:00:00.000Z","edited_at":"2026-05-20T10:00:00.000Z"}],
"updated_at":"2026-06-01T09:00:00.000Z"}`
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(current), jsontext.Value(incoming), repeatedImportLookups())
	require.NoError(t, err)

	events := []FoldEvent{
		testEvent("issue.snapshot", 1, current),
		testEvent("issue.commented", 5, `{"comment_uid":"comment-2","author":"carol","body":"original text","created_at":"2026-05-05T10:00:00.000Z"}`),
		testEvent("issue.comment_edited", 6, `{"comment_uid":"comment-2","body":"earlier edit","edited_at":"2026-05-10T10:00:00.000Z"}`),
	}
	for i, delta := range deltas {
		events = append(events, testEvent(delta.Type, int64(10+i), string(delta.Payload)))
	}
	comment := FoldEvents(events).Comments["comment-2"]
	assert.Equal(t, "latest text", comment.Body)
	assert.Equal(t, "2026-05-20T10:00:00.000Z", comment.EditedAt)
	assert.Equal(t, "carol", comment.Author)
	assert.Equal(t, "2026-05-05T10:00:00.000Z", comment.CreatedAt)
}

// A link from a winning copy must not close a parent cycle, and two copies in
// one push must not both add the same link.
func TestRepeatedSnapshotDeltasRefuseParentCycleAndRepeatedLink(t *testing.T) {
	lookups := &RepeatedSnapshotLookups{
		IssueKnown: func(string) (bool, error) { return true, nil },
		ParentOf: func(childUID string) (string, error) {
			if childUID == "issue-y" {
				return "issue-x", nil
			}
			return "", nil
		},
	}
	types := func(deltas []RepeatedSnapshotDelta) []string {
		var out []string
		for _, delta := range deltas {
			out = append(out, delta.Type)
		}
		return out
	}
	hubX := `{"uid":"issue-x","title":"x","status":"open","updated_at":"2026-05-01T09:00:00.000Z"}`
	// On the hub Y is a child of X. The newer copy makes X a child of Y.
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(hubX), jsontext.Value(
		`{"uid":"issue-x","title":"x","status":"open","links":[{"type":"parent","to_issue_uid":"issue-y"}],"updated_at":"2026-06-01T09:00:00.000Z"}`), lookups)
	require.NoError(t, err)
	assert.Equal(t, []string{RepeatedImportEventType}, types(deltas), "the cycle-closing parent link is left out")

	child := `{"uid":"issue-c","title":"c","status":"open","links":[{"type":"parent","to_issue_uid":"issue-a"}],"updated_at":"2026-06-01T09:00:00.000Z"}`
	parent := `{"uid":"issue-a","title":"a","status":"open","links":[{"type":"parent","to_issue_uid":"issue-c","incoming":true}],"updated_at":"2026-06-01T09:00:00.000Z"}`
	first, err := RepeatedSnapshotDeltas("importer", jsontext.Value(`{"uid":"issue-c","title":"c","status":"open","updated_at":"2026-05-01T09:00:00.000Z"}`), jsontext.Value(child), lookups)
	require.NoError(t, err)
	assert.Equal(t, []string{RepeatedImportEventType, "issue.linked"}, types(first))
	assert.Equal(t, "issue-a", first[1].RelatedIssueUID, "a link event names the other issue")
	second, err := RepeatedSnapshotDeltas("importer", jsontext.Value(`{"uid":"issue-a","title":"a","status":"open","updated_at":"2026-05-01T09:00:00.000Z"}`), jsontext.Value(parent), lookups)
	require.NoError(t, err)
	assert.Equal(t, []string{RepeatedImportEventType}, types(second), "the second copy does not write the same link again")
}

// The hub owns the recurring-work keys in issue metadata. A copy from another
// home never carries them, and applying it must not remove them.
func TestRepeatedSnapshotDeltasKeepHubOwnedRecurringMetadata(t *testing.T) {
	current := `{"uid":"issue-1","title":"t","status":"open","metadata":{"cron.occurrence":{"n":3},"cron.progress.job-1":{"n":3},"team":"core"},"updated_at":"2026-05-01T09:00:00.000Z"}`
	incoming := `{"uid":"issue-1","title":"t","status":"open","metadata":{"team":"platform"},"updated_at":"2026-06-01T09:00:00.000Z"}`
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(current), jsontext.Value(incoming), repeatedImportLookups())
	require.NoError(t, err)

	events := []FoldEvent{testEvent("issue.snapshot", 1, current)}
	for i, delta := range deltas {
		events = append(events, testEvent(delta.Type, int64(10+i), string(delta.Payload)))
	}
	assert.JSONEq(t, `{"cron.occurrence":{"n":3},"cron.progress.job-1":{"n":3},"team":"platform"}`,
		string(FoldEvents(events).IssueMetadata["issue-1"]))
}

// A comment's text follows its edit time, not the order of events. A later
// edit of the same comment that the copy never saw must win, whether its event
// replays before or after the hub's event for the copy.
func TestRepeatedSnapshotDeltasKeepALaterCommentEdit(t *testing.T) {
	current := `{"uid":"issue-1","title":"t","status":"open","comments":[
 {"comment_uid":"comment-1","author":"alice","body":"first","created_at":"2026-05-01T10:00:00.000Z"}],
"updated_at":"2026-05-01T10:00:00.000Z"}`
	incoming := `{"uid":"issue-1","title":"t","status":"open","comments":[
 {"comment_uid":"comment-1","author":"alice","body":"copy edit","created_at":"2026-05-01T10:00:00.000Z","edited_at":"2026-05-10T10:00:00.000Z"}],
"updated_at":"2026-05-11T09:00:00.000Z"}`
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(current), jsontext.Value(incoming), repeatedImportLookups())
	require.NoError(t, err)
	require.Len(t, deltas, 1)
	later := `{"comment_uid":"comment-1","body":"later edit","edited_at":"2026-05-20T10:00:00.000Z"}`
	for name, laterClock := range map[string]int64{"the later edit replays first": 5, "the later edit replays last": 20} {
		t.Run(name, func(t *testing.T) {
			comment := FoldEvents([]FoldEvent{
				testEvent("issue.snapshot", 1, current),
				testEvent("issue.comment_edited", laterClock, later),
				testEvent(deltas[0].Type, 10, string(deltas[0].Payload)),
			}).Comments["comment-1"]
			assert.Equal(t, "later edit", comment.Body)
			assert.Equal(t, "2026-05-20T10:00:00.000Z", comment.EditedAt)
		})
	}
}

// The hub decides by updated_at when the copy arrives, and replay must decide
// the same way whatever arrives afterwards. An edit made after the copy can
// reach the hub later and sort before the hub's event for the copy. The issue
// is then newer than the copy at that point of the replay, so the copy does
// not apply: this is the result the hub would have reached had the edit
// arrived first.
func TestRepeatedImportEventYieldsToANewerIssue(t *testing.T) {
	current := `{"uid":"issue-1","title":"hub title","status":"open","labels":["bug"],"updated_at":"2026-05-01T09:00:00.000Z"}`
	incoming := `{"uid":"issue-1","title":"copy title","status":"open","labels":["shipped"],"updated_at":"2026-06-01T09:00:00.000Z"}`
	deltas, err := RepeatedSnapshotDeltas("importer", jsontext.Value(current), jsontext.Value(incoming), repeatedImportLookups())
	require.NoError(t, err)
	require.Len(t, deltas, 1)

	got := FoldEvents([]FoldEvent{
		testEvent("issue.snapshot", 1, current),
		testEvent("issue.updated", 50, `{"title":"edited after the copy","updated_at":"2026-06-15T09:00:00.000Z"}`),
		testEvent(deltas[0].Type, 100, string(deltas[0].Payload)),
	})
	assert.Equal(t, "edited after the copy", got.Issues["issue-1"].Title)
	assert.Equal(t, "2026-06-15T09:00:00.000Z", got.Issues["issue-1"].UpdatedAt)
	assert.Equal(t, []string{"bug"}, presentLabels(got, "issue-1"), "a copy that does not apply changes nothing")

	// An older edit whose event clock is later than the copy's data time
	// replays before the hub's event, which sits at the hub's own clock.
	got = FoldEvents([]FoldEvent{
		testEvent("issue.snapshot", 1, current),
		testEvent("issue.updated", 50, `{"title":"older hub edit","updated_at":"2026-05-10T09:00:00.000Z"}`),
		testEvent(deltas[0].Type, 100, string(deltas[0].Payload)),
	})
	assert.Equal(t, "copy title", got.Issues["issue-1"].Title)
	assert.Equal(t, []string{"shipped"}, presentLabels(got, "issue-1"))
}

// A reader that predates repeated imports does not know the hub's event for a
// winning copy and would skip it. The event type therefore needs the repeated
// adoption feature, like a move needs its own.
func TestRepeatedImportEventRequiresTheFeature(t *testing.T) {
	assert.Equal(t, RepeatedAdoptionEventFeature, EventRequiredFeatures(RepeatedImportEventType))
	_, wire, err := FederationEventWireVersion(RepeatedImportEventType)
	require.NoError(t, err)
	assert.Equal(t, RepeatedAdoptionEventFeature, wire)
	assert.Empty(t, EventRequiredFeatures("issue.updated"))
}
