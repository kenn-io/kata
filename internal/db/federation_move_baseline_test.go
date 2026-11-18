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
		{UID: "newer-baseline", IssueUID: "b", Type: "issue.snapshot", HLCPhysicalMS: 3},
	}
	index := IndexFederatedMoveEvents(events)
	require.Len(t, index.Winners, 2)
	require.Equal(t, "latest-b", index.Winners["a"].UID)
	require.Equal(t, "newer-baseline", index.Winners["b"].UID)
	require.Len(t, index.Moves, 2)
	require.Equal(t, "latest-b", index.Moves["a"].UID)
	require.Equal(t, "second-issue", index.Moves["b"].UID, "a newer baseline does not retire the retained move")
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

// A whole copy that precedes the source baseline is part of what the baseline
// covers. Its comment text must not come back over the baseline's.
func TestFederationMoveBaselineCoversAnEarlierWholeCopy(t *testing.T) {
	comment := func(body, editedAt string) string {
		return `[{"comment_uid":"comment","author":"author","body":"` + body + `","created_at":"2026-01-01T00:00:00Z","edited_at":"` + editedAt + `"}]`
	}
	events := []FoldEvent{
		{UID: "first", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"uid":"issue","title":"first","comments":[{"comment_uid":"comment","author":"author","body":"first text","created_at":"2026-01-01T00:00:00Z"}]}`)},
		{UID: "copy", ProjectUID: "source", IssueUID: "issue", Type: RepeatedImportEventType, HLCPhysicalMS: 2, CreatedAt: "2026-01-02T00:00:00Z", Payload: jsontext.Value(`{"title":"copy title","status":"open","updated_at":"2026-01-01T12:00:00Z","comments":` + comment("copy text", "2026-01-01T01:00:00Z") + `}`)},
		{UID: "edit", ProjectUID: "source", IssueUID: "issue", Type: "issue.comment_edited", HLCPhysicalMS: 3, Payload: jsontext.Value(`{"comment_uid":"comment","body":"edited text","edited_at":"2026-01-03T00:00:00Z"}`)},
		{UID: "baseline", ProjectUID: "source", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 4, Payload: jsontext.Value(`{"uid":"issue","title":"copy title","status":"open","updated_at":"2026-01-03T00:00:00Z","comments":` + comment("edited text", "2026-01-03T00:00:00Z") + `}`)},
		{UID: "move", ProjectUID: "target", IssueUID: "issue", Type: "issue.moved", HLCPhysicalMS: 5, Payload: jsontext.Value(`{"from_project_uid":"source","to_project_uid":"target"}`)},
	}
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "edited text", projection.Comments["comment"].Body)
	require.Equal(t, "copy title", projection.Issues["issue"].Title)
}

// A destination snapshot that relocates an issue is its state at the
// snapshot's clock. The holding project's older labels and the comments the
// snapshot carries yield to it; spoke-local comments and newer changes stay.
func TestFederationRelocationBaselineCoversOlderSourceHistory(t *testing.T) {
	events := []FoldEvent{
		{UID: "creation", ProjectUID: "source", IssueUID: "issue", Type: "issue.created", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"uid":"issue","title":"original title","labels":["removed-on-hub"]}`)},
		{UID: "old-label", ProjectUID: "source", IssueUID: "issue", Type: "issue.labeled", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"label":"also-removed"}`)},
		{UID: "old-comment", ProjectUID: "source", IssueUID: "issue", Type: "issue.commented", HLCPhysicalMS: 2, HLCCounter: 1, Payload: jsontext.Value(`{"comment_uid":"comment","author":"worker","body":"original discussion","created_at":"2026-01-01T00:00:00Z"}`)},
		{UID: "local-comment", ProjectUID: "source", IssueUID: "issue", Type: "issue.commented", HLCPhysicalMS: 3, Payload: jsontext.Value(`{"comment_uid":"local","author":"worker","body":"spoke-only discussion","created_at":"2026-01-01T00:10:00Z"}`)},
		{UID: "baseline", ProjectUID: "target", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 5, Payload: jsontext.Value(`{"uid":"issue","title":"baseline title","comments":[{"comment_uid":"comment","author":"worker","body":"edited on the hub","created_at":"2026-01-01T00:00:00Z","edited_at":"2026-01-01T01:00:00Z"}]}`)},
		{UID: "newer-label", ProjectUID: "source", IssueUID: "issue", Type: "issue.labeled", HLCPhysicalMS: 6, Payload: jsontext.Value(`{"label":"newer"}`)},
	}
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.Equal(t, "target", projection.Issues["issue"].ProjectUID)
	require.Equal(t, "baseline title", projection.Issues["issue"].Title)
	require.Equal(t, "edited on the hub", projection.Comments["comment"].Body)
	require.Equal(t, "spoke-only discussion", projection.Comments["local"].Body)
	var present []string
	for key, state := range projection.Labels {
		if key.IssueUID == "issue" && state.Present {
			present = append(present, key.Label)
		}
	}
	require.Equal(t, []string{"newer"}, present)
}

// Within one project a snapshot merges with that project's own label history,
// so a spoke label the hub had not yet received survives a baseline refresh.
func TestFederationRelocationBaselineKeepsOwnProjectHistory(t *testing.T) {
	events := []FoldEvent{
		{UID: "creation", ProjectUID: "project", IssueUID: "issue", Type: "issue.created", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"uid":"issue","title":"title"}`)},
		{UID: "local-label", ProjectUID: "project", IssueUID: "issue", Type: "issue.labeled", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"label":"spoke-only"}`)},
		{UID: "baseline", ProjectUID: "project", IssueUID: "issue", Type: "issue.snapshot", HLCPhysicalMS: 3, Payload: jsontext.Value(`{"uid":"issue","title":"title"}`)},
	}
	projection := FoldEvents(FederationMoveBaselineEvents(events))
	require.True(t, projection.Labels[FoldLabelKey{IssueUID: "issue", Label: "spoke-only"}].Present)
}
