package tui

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
)

func TestThreadOrder(t *testing.T) {
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		comments []CommentEntry
		rows     []string
	}{
		{"nesting and root order", []CommentEntry{{UID: "b"}, {UID: "a"}, {UID: "c", Reply: &commentref.Link{UID: "a"}}, {UID: "d", Reply: &commentref.Link{UID: "c"}}}, []string{"b:0", "a:0", "c:1", "d:2"}},
		{"foreign siblings", []CommentEntry{
			{UID: "a", Backlinks: []commentref.Link{{UID: "early", CreatedAt: when}, {UID: "middle", CreatedAt: when.Add(2 * time.Minute)}, {UID: "late", CreatedAt: when.Add(4 * time.Minute)}}},
			{UID: "c", CreatedAt: when.Add(3 * time.Minute), Reply: &commentref.Link{UID: "a"}},
			{UID: "b", CreatedAt: when.Add(time.Minute), Reply: &commentref.Link{UID: "a"}},
		}, []string{"a:0", "a->early:1", "a->middle:1", "a->late:1", "b:1", "c:1"}},
		{"tied times", []CommentEntry{
			{UID: "a", Backlinks: []commentref.Link{{UID: "c", CreatedAt: when}, {UID: "b", CreatedAt: when}}},
			{UID: "d", CreatedAt: when, Reply: &commentref.Link{UID: "a"}},
			{UID: "b", CreatedAt: when, Reply: &commentref.Link{UID: "a"}},
		}, []string{"a:0", "a->c:1", "b:1", "d:1"}},
		{"unknown parent", []CommentEntry{{UID: "c", Reply: &commentref.Link{UID: "unknown"}}}, []string{"c:0"}},
		{"empty UIDs", []CommentEntry{{Backlinks: []commentref.Link{{UID: "foreign"}}}, {}}, []string{":0", ":0"}},
		{"empty UID reply", []CommentEntry{{UID: "a"}, {Reply: &commentref.Link{UID: "a"}}}, []string{"a:0", ":0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var rows []string
			for _, c := range threadOrder(tt.comments) {
				rows = append(rows, fmt.Sprintf("%s:%d", c.UID, c.depth))
				for _, leaf := range c.leaves {
					rows = append(rows, fmt.Sprintf("%s->%s:%d", c.UID, leaf.link.UID, leaf.depth))
				}
			}
			require.Equal(t, tt.rows, rows)
		})
	}
	chain := []CommentEntry{{UID: "0"}}
	for i := 1; i < 7; i++ {
		chain = append(chain, CommentEntry{UID: fmt.Sprint(i), Reply: &commentref.Link{UID: fmt.Sprint(i - 1)}})
	}
	chain[6].Backlinks = []commentref.Link{{UID: "foreign"}}
	got := threadOrder(chain)
	require.Equal(t, []int{0, 1, 2, 3, 4, 4, 4}, []int{got[0].depth, got[1].depth, got[2].depth, got[3].depth, got[4].depth, got[5].depth, got[6].depth})
	require.Equal(t, 4, got[6].leaves[0].depth)
}

func TestCommentTreePickerLeaves(t *testing.T) {
	comments := threadOrder([]CommentEntry{{UID: "a", Body: "Finding", Reply: &commentref.Link{UID: "parent", IssueUID: "other", Kind: "confirm", Body: "Parent evidence"}, Backlinks: []commentref.Link{
		{UID: "b", IssueUID: "other", Kind: "confirm", Author: "worker-b", Body: "First confirmation"},
		{UID: "c", IssueUID: "other", Kind: "confirm", Author: "worker-c", Body: "Second confirmation"},
		{UID: "d", IssueUID: "other", Kind: "refute", Author: "worker-d", Body: "Refutation evidence"},
	}}})
	for _, tt := range []struct {
		name         string
		cursor       int
		linkCursor   int
		confirmation bool
		refutation   bool
	}{
		{"unselected", -1, 1, false, false},
		{"confirmations", 0, 1, true, false},
		{"refutation", 0, 3, false, true},
		{"outgoing reply", 0, 0, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chunks := commentChunks(comments, 100, tt.cursor, tabState{commentLinkCursor: tt.linkCursor})
			rendered := stripANSI(strings.Join(chunks[0].lines, "\n"))
			require.Equal(t, tt.confirmation, strings.Contains(rendered, "First confirmation"))
			require.Equal(t, tt.confirmation, strings.Contains(rendered, "Second confirmation"))
			require.Equal(t, tt.refutation, strings.Contains(rendered, "Refutation evidence"))
		})
	}
}

func TestCommentTags(t *testing.T) {
	for _, tt := range []struct {
		name  string
		links []commentref.Link
		want  string
	}{
		{"confirm", []commentref.Link{{Kind: "confirm"}}, "confirm"},
		{"both", []commentref.Link{{Kind: "refute"}, {Kind: "confirm"}}, "confirm · refute"},
		{"stale", []commentref.Link{{Kind: "confirm", TargetEdited: true}, {Kind: "refute", TargetEdited: true}}, "confirm (before edit) · refute (before edit)"},
		{"mixed", []commentref.Link{{Kind: "confirm", TargetEdited: true}, {Kind: "confirm"}}, "confirm"},
		{"other kinds", []commentref.Link{{Kind: "reply"}, {Kind: "supersede"}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, stripANSI(commentTags(tt.links)))
		})
	}
}

func TestCommentTreeColoredTags(t *testing.T) {
	t.Setenv("KATA_COLOR_MODE", "dark")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode()
	t.Cleanup(applyDefaultColorMode)
	c := CommentEntry{depth: 4, Backlinks: []commentref.Link{
		{Kind: "confirm", TargetEdited: true}, {Kind: "refute", TargetEdited: true},
	}}
	require.Contains(t, commentTags(c.Backlinks), "\x1b[")
	for width := 30; width <= 79; width++ {
		chunks := commentChunks([]CommentEntry{c}, width, -1, tabState{})
		plain := stripANSI(strings.Join(chunks[0].lines[1:], "\n"))
		require.NotContains(t, plain, "\x1b", "width %d", width)
		require.Contains(t, strings.Join(strings.Fields(plain), ""), "confirm(beforeedit)·refute(beforeedit)", "width %d", width)
	}
}

func TestCommentTreeNarrowIndent(t *testing.T) {
	defer snapshotInit(t)()
	comments := []CommentEntry{{UID: "a", Handle: "c:a", Body: "Read the evidence.", depth: 4,
		leaves: []commentLeaf{{link: commentref.Link{Handle: "b:reply", Kind: "confirm"}, depth: 4}},
	}}
	chunks := commentChunks(comments, 20, -1, tabState{})
	assertGolden(t, "detail-comment-tree-tiny", strings.Join(chunks[0].lines, "\n"))
}

func TestReviewNestedEditedHeader(t *testing.T) {
	defer snapshotInit(t)()
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	edited := when.Add(time.Minute)
	comments := []CommentEntry{{UID: "0", Handle: "c:0", Author: "review-bot", CreatedAt: when}}
	for i := 1; i <= 4; i++ {
		comments = append(comments, CommentEntry{UID: fmt.Sprint(i), Handle: fmt.Sprintf("c:%d", i), Author: "review-bot", CreatedAt: when, Reply: &commentref.Link{UID: fmt.Sprint(i - 1), Kind: "reply"}})
	}
	comments[4].Handle, comments[4].EditedAt = "c:abcd", &edited
	rendered := stripANSI(renderCommentsTab(threadOrder(comments), 50, 100, 4, tabState{}))
	require.Contains(t, rendered, "c:abcd  review-bot  Apr 30 10:00  (edited)")
}

func TestSnapshot_CommentTree(t *testing.T) {
	defer snapshotInit(t)()
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	edited := when.Add(time.Minute)
	dm := snapDetailFixture()
	dm.issue.Author, dm.issue.Title, dm.issue.Body = "worker", "Review a finding", "Discuss the evidence."
	dm.activeTab, dm.detailFocus = tabComments, focusActivity
	projected := commentref.Project([]commentref.Record{
		{UID: "a", Handle: "c:root", IssueUID: "issue-a", Author: "worker-a", Body: "A finding to inspect.", CreatedAt: when, EditedAt: &edited},
		{UID: "foreign", Handle: "c:reply", IssueUID: "issue-b", IssueShortID: "bbbb", ReplyToUID: "a", ReplyKind: "confirm", Author: "worker-b", Body: "Foreign evidence.", CreatedAt: when, EditedAt: &edited},
		{UID: "b", Handle: "c:reply", IssueUID: "issue-a", ReplyToUID: "a", ReplyKind: "refute", Author: "worker-c", Body: "Local evidence.", CreatedAt: when.Add(2 * time.Minute), EditedAt: &edited},
		{UID: "c", Handle: "c:nested", IssueUID: "issue-a", ReplyToUID: "b", ReplyKind: "confirm", Author: "worker-d", Body: "Confirmed.", CreatedAt: when.Add(3 * time.Minute)},
	}, nil)
	selected, err := commentref.Select(projected, "issue-a", commentref.Options{})
	require.NoError(t, err)
	wire, err := json.Marshal(selected.Comments)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &dm.comments))
	dm.comments = threadOrder(dm.comments)
	assertGolden(t, "detail-comment-tree", dm.View(80, 60, viewChrome{}))
	records := []commentref.Record{{UID: "0", Handle: "c:0", IssueUID: "issue-a", Author: "worker", Body: "Start.", CreatedAt: when}}
	for i := 1; i < 7; i++ {
		records = append(records, commentref.Record{UID: fmt.Sprint(i), Handle: fmt.Sprintf("c:%d", i), IssueUID: "issue-a", Author: "worker", Body: "Evidence remains visible at depth.", CreatedAt: when.Add(time.Duration(i) * time.Minute), ReplyToUID: fmt.Sprint(i - 1), ReplyKind: "confirm"})
	}
	records[5].EditedAt = &edited
	selected, err = commentref.Select(commentref.Project(records, nil), "issue-a", commentref.Options{})
	require.NoError(t, err)
	wire, err = json.Marshal(selected.Comments)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &dm.comments))
	dm.comments = threadOrder(dm.comments)
	dm.tabCursor = 6
	assertGolden(t, "detail-comment-tree-narrow", dm.View(50, 60, viewChrome{}))
}

func TestCommentTreeModelWalk(t *testing.T) {
	m := newTestModel()
	m.view = viewDetail
	m.detail = detailModel{issue: &Issue{UID: "issue-a", ShortID: "aaaa", Status: "open"}, scopePID: 7, activeTab: tabComments, detailFocus: focusActivity, gen: 1}
	m.nextGen = 1
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a", Backlinks: []commentref.Link{{UID: "b", IssueUID: "issue-a", ProjectID: 7, Kind: "confirm"}, {UID: "foreign", IssueUID: "issue-b", ProjectID: 7, Kind: "refute"}}},
		{UID: "other", Handle: "c:other"},
		{UID: "b", Handle: "c:b", Reply: &commentref.Link{UID: "a", IssueUID: "issue-a", Kind: "confirm"}},
	}
	m, _ = updateModel(m, commentsFetchedMsg{gen: 1, comments: comments})
	for _, want := range []string{"b", "other"} {
		m, _ = updateModel(m, runeKey('j'))
		require.Equal(t, want, m.detail.comments[m.detail.tabCursor].UID)
	}
	for _, want := range []string{"b", "a"} {
		m, _ = updateModel(m, runeKey('k'))
		require.Equal(t, want, m.detail.comments[m.detail.tabCursor].UID)
	}
}

func TestCommentRefreshBeforeIssueFetched(t *testing.T) {
	dm := detailModel{
		activeTab: tabComments, detailFocus: focusActivity, gen: 1,
		lastTermWidth: 80, lastTermHeight: 24,
		comments: []CommentEntry{{UID: "a", Handle: "c:a"}}, tabCursor: 0,
	}
	require.NotPanics(t, func() {
		dm = dm.applyFetched(commentsFetchedMsg{gen: 1, comments: []CommentEntry{{UID: "a", Handle: "c:a"}}})
	})
}

func TestCommentRefresh(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint("manual=", manual), func(t *testing.T) {
			dm := detailModel{issue: &Issue{UID: "issue-a", Status: "open"}, activeTab: tabComments, detailFocus: focusActivity, gen: 1, lastTermWidth: 80, lastTermHeight: 24, comments: []CommentEntry{{UID: "a", Handle: "c:a"}, {UID: "b", Handle: "c:b"}}, tabCursor: 1}
			if manual {
				dm.issue.Body = strings.Repeat("description line\n", 50)
			} else {
				dm = dm.revealCursor()
				require.Contains(t, stripANSI(dm.View(80, 24, viewChrome{})), "> c:b")
			}
			dm = dm.applyFetched(commentsFetchedMsg{gen: 1, comments: []CommentEntry{{UID: "a", Handle: "c:a"}, {UID: "b", Handle: "c:b"}, {UID: "reply", Body: strings.Repeat("reply line\n", 60), Reply: &commentref.Link{UID: "a", Kind: "reply"}}}})
			require.Equal(t, "b", dm.comments[dm.tabCursor].UID)
			if manual {
				require.Zero(t, dm.scroll)
			} else {
				require.Contains(t, stripANSI(dm.View(80, 24, viewChrome{})), "> c:b")
			}
		})
	}
}
