package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPresentLabelsSkipsTombstonedEdges(t *testing.T) {
	projection := FoldProjection{Labels: map[FoldLabelKey]FoldElementState{
		{IssueUID: "issue-a", Label: "kept"}:    {Present: true},
		{IssueUID: "issue-a", Label: "removed"}: {Present: false},
	}}
	assert.Equal(t, []FoldLabelKey{{IssueUID: "issue-a", Label: "kept"}},
		projection.PresentLabels(),
		"an added-then-removed label must not be materialized")
}

func TestPresentLinksSkipsTombstonedEdges(t *testing.T) {
	kept := FoldLinkKey{FromUID: "issue-a", ToUID: "issue-b", Type: "blocks"}
	removed := FoldLinkKey{FromUID: "issue-a", ToUID: "issue-c", Type: "blocks"}
	keptState := FoldElementState{
		Present: true,
		Clock: FoldClock{
			HLCPhysicalMS:     123,
			HLCCounter:        4,
			OriginInstanceUID: "origin-a",
			EventUID:          "event-a",
		},
		Author: "remote",
	}
	projection := FoldProjection{Links: map[FoldLinkKey]FoldElementState{
		kept:    keptState,
		removed: {Present: false, Author: "remote"},
	}}
	edges := projection.PresentLinks()
	require.Len(t, edges, 1)
	assert.Equal(t, kept, edges[0].Key)
	assert.Equal(t, keptState, edges[0].State,
		"the accessor must hand back the complete state, not just the key")
}

func TestPresentLabelsOrdersByIssueThenLabel(t *testing.T) {
	projection := FoldProjection{Labels: map[FoldLabelKey]FoldElementState{
		{IssueUID: "issue-b", Label: "alpha"}: {Present: true},
		{IssueUID: "issue-a", Label: "zulu"}:  {Present: true},
		{IssueUID: "issue-a", Label: "alpha"}: {Present: true},
	}}
	assert.Equal(t, []FoldLabelKey{
		{IssueUID: "issue-a", Label: "alpha"},
		{IssueUID: "issue-a", Label: "zulu"},
		{IssueUID: "issue-b", Label: "alpha"},
	}, projection.PresentLabels())
}

func TestPresentLinksOrdersByFromThenToThenType(t *testing.T) {
	projection := FoldProjection{Links: map[FoldLinkKey]FoldElementState{
		{FromUID: "issue-a", ToUID: "issue-b", Type: "related"}: {Present: true},
		{FromUID: "issue-a", ToUID: "issue-b", Type: "blocks"}:  {Present: true},
		{FromUID: "issue-a", ToUID: "issue-a", Type: "parent"}:  {Present: true},
		{FromUID: "issue-b", ToUID: "issue-a", Type: "blocks"}:  {Present: true},
	}}
	var got []FoldLinkKey
	for _, edge := range projection.PresentLinks() {
		got = append(got, edge.Key)
	}
	assert.Equal(t, []FoldLinkKey{
		{FromUID: "issue-a", ToUID: "issue-a", Type: "parent"},
		{FromUID: "issue-a", ToUID: "issue-b", Type: "blocks"},
		{FromUID: "issue-a", ToUID: "issue-b", Type: "related"},
		{FromUID: "issue-b", ToUID: "issue-a", Type: "blocks"},
	}, got)
}

func TestSortedIssuesAndCommentsOrderByUID(t *testing.T) {
	projection := FoldProjection{
		Issues: map[string]FoldIssue{
			"uid-c": {UID: "uid-c", Title: "third"},
			"uid-a": {UID: "uid-a", Title: "first"},
			"uid-b": {UID: "uid-b", Title: "second"},
		},
		Comments: map[string]FoldComment{
			"comment-b": {UID: "comment-b", IssueUID: "uid-a"},
			"comment-a": {UID: "comment-a", IssueUID: "uid-a"},
		},
	}
	issues := projection.SortedIssues()
	require.Len(t, issues, 3)
	assert.Equal(t, []string{"uid-a", "uid-b", "uid-c"},
		[]string{issues[0].UID, issues[1].UID, issues[2].UID})
	assert.Equal(t, "first", issues[0].Title, "values, not just keys")

	comments := projection.SortedComments()
	require.Len(t, comments, 2)
	assert.Equal(t, []string{"comment-a", "comment-b"},
		[]string{comments[0].UID, comments[1].UID})
}

// Go randomizes map iteration order, so building the same logical projection
// many times and asserting one answer is what pins determinism. Without a
// stable order, federated short_id assignment is nondeterministic on replay.
func TestAccessorsAreStableAcrossRandomizedInsertion(t *testing.T) {
	var wantIssues []string
	var wantLabels []FoldLabelKey
	var wantLinks []FoldLinkKey
	for attempt := range 50 {
		projection := FoldProjection{
			Issues:   map[string]FoldIssue{},
			Labels:   map[FoldLabelKey]FoldElementState{},
			Links:    map[FoldLinkKey]FoldElementState{},
			Comments: map[string]FoldComment{},
		}
		for i := range 12 {
			uid := fmt.Sprintf("uid-%02d", (i*7)%12)
			projection.Issues[uid] = FoldIssue{UID: uid}
			projection.Labels[FoldLabelKey{IssueUID: uid, Label: fmt.Sprintf("l%02d", i)}] =
				FoldElementState{Present: i%3 != 0}
			projection.Links[FoldLinkKey{FromUID: uid, ToUID: "uid-00", Type: "blocks"}] =
				FoldElementState{Present: true}
		}

		var gotIssues []string
		for _, issue := range projection.SortedIssues() {
			gotIssues = append(gotIssues, issue.UID)
		}
		gotLabels := projection.PresentLabels()
		var gotLinks []FoldLinkKey
		for _, edge := range projection.PresentLinks() {
			gotLinks = append(gotLinks, edge.Key)
		}

		if attempt == 0 {
			wantIssues, wantLabels, wantLinks = gotIssues, gotLabels, gotLinks
			require.NotEmpty(t, wantLabels, "fixture must contain present labels")
			continue
		}
		assert.Equal(t, wantIssues, gotIssues)
		assert.Equal(t, wantLabels, gotLabels)
		assert.Equal(t, wantLinks, gotLinks)
	}
}

func TestAccessorsReturnEmptyNotNilForEmptyProjection(t *testing.T) {
	projection := FoldEvents(nil)
	issues := projection.SortedIssues()
	comments := projection.SortedComments()
	labels := projection.PresentLabels()
	links := projection.PresentLinks()
	require.NotNil(t, issues)
	require.NotNil(t, comments)
	require.NotNil(t, labels)
	require.NotNil(t, links)
	assert.Empty(t, issues)
	assert.Empty(t, comments)
	assert.Empty(t, labels)
	assert.Empty(t, links)
}
