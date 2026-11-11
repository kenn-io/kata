package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndexFederatedMoveEvents(t *testing.T) {
	events := []FoldEvent{
		{UID: "latest-b", IssueUID: "a", Type: "issue.moved", HLCPhysicalMS: 10, HLCCounter: 1, OriginInstanceUID: "b"},
		{UID: "later-edit", IssueUID: "a", Type: "issue.updated", HLCPhysicalMS: 20},
		{UID: "older", IssueUID: "a", Type: "issue.moved", HLCPhysicalMS: 9},
		{UID: "latest-a", IssueUID: "a", Type: "issue.moved", HLCPhysicalMS: 10, HLCCounter: 1, OriginInstanceUID: "a"},
		{UID: "second-issue", IssueUID: "b", Type: "issue.moved", HLCPhysicalMS: 2},
	}
	winners := IndexFederatedMoveEvents(events)
	require.Len(t, winners, 2)
	require.Equal(t, "latest-b", winners["a"].UID)
	require.Equal(t, "second-issue", winners["b"].UID)
}

func TestFederationMoveBaselineUsesLatestApplicableSourceSnapshot(t *testing.T) {
	events := []FoldEvent{
		{UID: "creation", ProjectUID: "source", IssueUID: "issue", Type: "issue.created", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"uid":"issue","author":"original-author","title":"original title"}`)},
		{UID: "old-comment", ProjectUID: "source", IssueUID: "issue", Type: "issue.commented", HLCPhysicalMS: 1, HLCCounter: 1, Payload: jsontext.Value(`{"comment_uid":"comment","author":"original-author","body":"original discussion","created_at":"2026-01-01T00:00:00Z"}`)},
		{UID: "old-comment-edit", ProjectUID: "source", IssueUID: "issue", Type: "issue.comment_edited", HLCPhysicalMS: 2, HLCCounter: 1, Payload: jsontext.Value(`{"comment_uid":"comment","body":"before rewrite discussion","edited_at":"2026-01-01T00:30:00Z"}`)},
		{UID: "first-enrollment", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"uid":"issue","author":"first-author","title":"first title"}`)},
		{UID: "reenrollment", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 3, Payload: jsontext.Value(`{"uid":"issue","author":"renamed-author","title":"baseline title","comments":[{"comment_uid":"comment","author":"renamed-author","body":"baseline discussion","edited_at":"2026-01-01T01:00:00Z"}]}`)},
		{UID: "source-edit", ProjectUID: "source", IssueUID: "issue", Type: "issue.updated", HLCPhysicalMS: 4, Payload: jsontext.Value(`{"title":"later title"}`)},
		{UID: "move", ProjectUID: "target", IssueUID: "issue", Type: "issue.moved", HLCPhysicalMS: 5, Payload: jsontext.Value(`{"from_project_uid":"source","to_project_uid":"target"}`)},
	}
	// Arrival order is independent of the source baseline's logical order.
	events[3], events[4] = events[4], events[3]
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "renamed-author", projection.Issues["issue"].Author)
	require.Equal(t, "later title", projection.Issues["issue"].Title)
	require.Equal(t, "target", projection.Issues["issue"].ProjectUID)
	require.Equal(t, "renamed-author", projection.Comments["comment"].Author)
	require.Equal(t, "baseline discussion", projection.Comments["comment"].Body)
	events = append(events, FoldEvent{UID: "later-comment-edit", ProjectUID: "target", IssueUID: "issue", Type: "issue.comment_edited", HLCPhysicalMS: 6, Payload: jsontext.Value(`{"comment_uid":"comment","body":"later discussion"}`)})
	projection = FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "renamed-author", projection.Comments["comment"].Author)
	require.Equal(t, "later discussion", projection.Comments["comment"].Body)
}

func TestFederationMoveBaselineDoesNotUseNestedMoveSnapshot(t *testing.T) {
	events := []FoldEvent{
		{UID: "creation", ProjectUID: "source", IssueUID: "issue", Type: "issue.created", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"uid":"issue","author":"original-author","title":"original title"}`)},
		{UID: "move", ProjectUID: "target", IssueUID: "issue", Type: "issue.moved", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"from_project_uid":"source","to_project_uid":"target","snapshot":{"uid":"issue","author":"forged-author","title":"injected title"}}`)},
	}
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "original-author", projection.Issues["issue"].Author)
	require.Equal(t, "original title", projection.Issues["issue"].Title)
}

func TestFederationMoveBaselinePreservesUncoveredCommentHistory(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		comment    string
		eventType  string
		payload    string
		createdAt  string
		wantBody   string
		wantAuthor string
	}{
		{name: "absent comment", comment: `[]`, eventType: "issue.commented", payload: `{"comment_uid":"comment","author":"delayed-author","body":"delayed discussion","created_at":"2026-01-01T00:00:00Z"}`, wantBody: "delayed discussion", wantAuthor: "delayed-author"},
		{name: "newer delayed edit", comment: `[{"comment_uid":"comment","author":"renamed-author","body":"baseline discussion","created_at":"2026-01-01T00:00:00Z","edited_at":"2026-01-01T01:00:00Z"}]`, eventType: "issue.comment_edited", payload: `{"comment_uid":"comment","body":"delayed discussion","edited_at":"2026-01-01T02:00:00Z"}`, wantBody: "delayed discussion", wantAuthor: "renamed-author"},
		{name: "equal timestamp different body", comment: `[{"comment_uid":"comment","author":"renamed-author","body":"baseline discussion","created_at":"2026-01-01T00:00:00Z","edited_at":"2026-01-01T01:00:00Z"}]`, eventType: "issue.comment_edited", payload: `{"comment_uid":"comment","body":"delayed discussion","edited_at":"2026-01-01T01:00:00Z"}`, wantBody: "delayed discussion", wantAuthor: "renamed-author"},
		{name: "covered edit", comment: `[{"comment_uid":"comment","author":"renamed-author","body":"baseline discussion","created_at":"2026-01-01T00:00:00Z","edited_at":"2026-01-01T01:00:00Z"}]`, eventType: "issue.comment_edited", payload: `{"comment_uid":"comment","body":"old discussion","edited_at":"2026-01-01T00:30:00Z"}`, wantBody: "baseline discussion", wantAuthor: "renamed-author"},
		{name: "created at fallback", comment: `[{"comment_uid":"comment","author":"renamed-author","body":"baseline discussion","created_at":"2026-01-01T00:00:00Z","edited_at":"2026-01-01T01:00:00Z"}]`, eventType: "issue.comment_edited", payload: `{"comment_uid":"comment","body":"delayed discussion"}`, createdAt: "2026-01-01T02:00:00Z", wantBody: "delayed discussion", wantAuthor: "renamed-author"},
		{name: "unknown baseline edit version", comment: `[{"comment_uid":"comment","author":"renamed-author","body":"baseline discussion","created_at":"2026-01-01T00:00:00Z"}]`, eventType: "issue.comment_edited", payload: `{"comment_uid":"comment","body":"delayed discussion","edited_at":"2026-01-01T00:30:00Z"}`, wantBody: "delayed discussion", wantAuthor: "renamed-author"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			events := []FoldEvent{
				{UID: "creation", ProjectUID: "source", IssueUID: "issue", Type: "issue.created", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"uid":"issue","author":"original-author"}`)},
				{UID: "baseline", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 3, Payload: jsontext.Value(`{"uid":"issue","author":"renamed-author","comments":` + scenario.comment + `}`)},
				{UID: "delayed", ProjectUID: "source", IssueUID: "issue", Type: scenario.eventType, HLCPhysicalMS: 2, CreatedAt: scenario.createdAt, Payload: jsontext.Value(scenario.payload)},
				{UID: "move", ProjectUID: "target", IssueUID: "issue", Type: "issue.moved", HLCPhysicalMS: 4, Payload: jsontext.Value(`{"from_project_uid":"source","to_project_uid":"target"}`)},
			}
			projection := FoldEvents(FederationMoveBaselineEvents(events))
			require.Equal(t, scenario.wantBody, projection.Comments["comment"].Body)
			require.Equal(t, scenario.wantAuthor, projection.Comments["comment"].Author)
		})
	}
}

func TestFederationMoveBaselineCoveredEditWithEqualTimestamp(t *testing.T) {
	events := []FoldEvent{
		{UID: "creation", ProjectUID: "source", IssueUID: "issue", Type: "issue.created", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"uid":"issue","author":"original-author"}`)},
		{UID: "old-edit", ProjectUID: "source", IssueUID: "issue", Type: "issue.comment_edited", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"comment_uid":"comment","body":"old discussion","edited_at":"2026-01-01T01:00:00Z"}`)},
		{UID: "covered-edit", ProjectUID: "source", IssueUID: "issue", Type: "issue.comment_edited", HLCPhysicalMS: 2, HLCCounter: 1, Payload: jsontext.Value(`{"comment_uid":"comment","body":"baseline discussion","edited_at":"2026-01-01T01:00:00Z"}`)},
		{UID: "baseline", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 3, Payload: jsontext.Value(`{"uid":"issue","author":"renamed-author","comments":[{"comment_uid":"comment","author":"renamed-author","body":"baseline discussion","created_at":"2026-01-01T00:00:00Z","edited_at":"2026-01-01T01:00:00Z"}]}`)},
		{UID: "move", ProjectUID: "target", IssueUID: "issue", Type: "issue.moved", HLCPhysicalMS: 4, Payload: jsontext.Value(`{"from_project_uid":"source","to_project_uid":"target"}`)},
	}
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "baseline discussion", projection.Comments["comment"].Body)
	require.Equal(t, "renamed-author", projection.Comments["comment"].Author)
	events = append(events, FoldEvent{UID: "later-skewed-edit", ProjectUID: "source", IssueUID: "issue", Type: "issue.comment_edited", HLCPhysicalMS: 2, HLCCounter: 2, Payload: jsontext.Value(`{"comment_uid":"comment","body":"later logical discussion","edited_at":"2026-01-01T00:30:00Z"}`)})
	projection = FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "later logical discussion", projection.Comments["comment"].Body, "known coverage clock takes precedence over wall time")
}

// The spoke that pushed an older copy of an issue still holds its own
// snapshot. When the issue later moves, that snapshot may be the latest by
// clock, and it must not become the baseline.
func TestFederationMoveBaselineKeepsRepeatedImportFreshness(t *testing.T) {
	events := []FoldEvent{
		{UID: "current-copy", OriginInstanceUID: "spoke-a", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"uid":"issue","author":"alice","title":"current title","labels":["kept"],"metadata":{"team":"core"},"comments":[{"comment_uid":"current-comment","author":"alice","body":"only in the current copy","created_at":"2026-05-01T10:00:00.000Z"}],"updated_at":"2026-06-01T09:00:00.000Z"}`)},
		{UID: "stale-copy", OriginInstanceUID: "spoke-b", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 3, Payload: jsontext.Value(`{"uid":"issue","author":"alice","title":"stale title","labels":["ancient"],"updated_at":"2026-05-01T09:00:00.000Z"}`)},
		{UID: "move", OriginInstanceUID: "hub", ProjectUID: "target", IssueUID: "issue", Type: "issue.moved", HLCPhysicalMS: 5, Payload: jsontext.Value(`{"from_project_uid":"source","to_project_uid":"target"}`)},
	}
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "current title", projection.Issues["issue"].Title)
	require.Equal(t, "target", projection.Issues["issue"].ProjectUID)
	require.True(t, projection.Labels[FoldLabelKey{IssueUID: "issue", Label: "kept"}].Present)
	require.False(t, projection.Labels[FoldLabelKey{IssueUID: "issue", Label: "ancient"}].Present)
	require.JSONEq(t, `{"team":"core"}`, string(projection.IssueMetadata["issue"]))
	require.Equal(t, "only in the current copy", projection.Comments["current-comment"].Body)
}

// An issue created on the hub has no enrollment snapshot. On the spoke that
// pushed an older copy, that copy is then the only snapshot before a move, and
// it must not replace the creation history it is older than.
func TestFederationMoveBaselineKeepsCreationBeforeStaleImport(t *testing.T) {
	events := []FoldEvent{
		{UID: "creation", OriginInstanceUID: "hub", ProjectUID: "source", IssueUID: "issue", Type: "issue.created", HLCPhysicalMS: 1, CreatedAt: "2026-06-01T08:00:00.000Z", Payload: jsontext.Value(`{"uid":"issue","author":"alice","title":"first title","created_at":"2026-06-01T08:00:00.000Z"}`)},
		{UID: "edit", OriginInstanceUID: "hub", ProjectUID: "source", IssueUID: "issue", Type: "issue.updated", HLCPhysicalMS: 2, CreatedAt: "2026-06-01T09:00:00.000Z", Payload: jsontext.Value(`{"title":"current title"}`)},
		{UID: "stale-copy", OriginInstanceUID: "spoke-b", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 3, CreatedAt: "2026-06-01T10:00:00.000Z", Payload: jsontext.Value(`{"uid":"issue","author":"alice","title":"stale title","updated_at":"2026-05-01T09:00:00.000Z"}`)},
		{UID: "move", OriginInstanceUID: "hub", ProjectUID: "target", IssueUID: "issue", Type: "issue.moved", HLCPhysicalMS: 5, CreatedAt: "2026-06-01T11:00:00.000Z", Payload: jsontext.Value(`{"from_project_uid":"source","to_project_uid":"target"}`)},
	}
	require.Equal(t, "current title", FoldEvents(events).Issues["issue"].Title, "the full history keeps the edit")
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "current title", projection.Issues["issue"].Title)
	require.Equal(t, "target", projection.Issues["issue"].ProjectUID)
}
