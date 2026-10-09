package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestSnapshotCommentAnnotationsUseCapturedGraph(t *testing.T) {
	created := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	edited := created.Add(time.Hour)
	finding := db.Comment{UID: "01AAAAAAAAAAAAAAAAAAAAAAAA", IssueID: 1, Body: "Captured finding", CreatedAt: created, EditedAt: &edited}
	reply := db.Comment{UID: "01BBBBBBBBBBBBBBBBBBBBBBBB", IssueID: 2, Body: "Answer", CreatedAt: created.Add(time.Minute), ReplyToUID: finding.UID, ReplyKind: "reply"}
	data := db.UISnapshotData{Cursor: 7, SelectedIssue: &db.UIIssue{UID: "target", ID: 1}, CommentGraph: db.CommentGraphData{Comments: []db.CommentGraphRecord{{Comment: finding, IssueUID: "target", IssueShortID: "aaaa"}, {Comment: reply, IssueUID: "source", IssueShortID: "bbbb"}}}}
	response, err := snapshotResponse(t.Context(), data, normalizedUISnapshotIntent{SelectedIssueUID: "target"}, uiPolicy{}, "validator")
	require.NoError(t, err)
	require.Equal(t, int64(7), response.Body.Cursor)
	require.Len(t, response.Body.Selected.Comments, 1)
	require.Equal(t, "Captured finding", response.Body.Selected.Comments[0].Body)
	require.Len(t, response.Body.Selected.Comments[0].Backlinks, 1)
	require.Equal(t, "bbbb:bbbbbb", response.Body.Selected.Comments[0].Backlinks[0].Handle)
	data.SelectedIssue = &db.UIIssue{UID: "source", ID: 2}
	response, err = snapshotResponse(t.Context(), data, normalizedUISnapshotIntent{SelectedIssueUID: "source"}, uiPolicy{}, "validator")
	require.NoError(t, err)
	require.True(t, response.Body.Selected.Comments[0].Reply.TargetEdited)
	cache := newUISnapshotEnrichmentCache()
	cache.put("entry", data)
	first, ok := cache.get("entry")
	require.True(t, ok)
	first.CommentGraph.Comments[0].Comment.Body = "Changed after read"
	second, ok := cache.get("entry")
	require.True(t, ok)
	require.Equal(t, "Captured finding", second.CommentGraph.Comments[0].Comment.Body)
}
