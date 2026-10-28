package tui

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
		{"no links", []CommentEntry{{UID: "b"}, {UID: "a"}}, []string{"b:0", "a:0"}},
		{"nesting and root order", []CommentEntry{{UID: "b"}, {UID: "a"}, {UID: "c", Reply: &commentref.Link{UID: "a"}}, {UID: "d", Reply: &commentref.Link{UID: "c"}}}, []string{"b:0", "a:0", "c:1", "d:2"}},
		{"foreign siblings", []CommentEntry{
			{UID: "a", Backlinks: []commentref.Link{{UID: "early", CreatedAt: when}, {UID: "middle", CreatedAt: when.Add(2 * time.Minute)}, {UID: "late", CreatedAt: when.Add(4 * time.Minute)}}},
			{UID: "c", CreatedAt: when.Add(3 * time.Minute), Reply: &commentref.Link{UID: "a"}},
			{UID: "b", CreatedAt: when.Add(time.Minute), Reply: &commentref.Link{UID: "a"}},
		}, []string{"a:0", "early:1", "b:1", "middle:1", "c:1", "late:1"}},
		{"tied times", []CommentEntry{
			{UID: "a", Backlinks: []commentref.Link{{UID: "c", CreatedAt: when}, {UID: "b", CreatedAt: when}}},
			{UID: "d", CreatedAt: when, Reply: &commentref.Link{UID: "a"}},
			{UID: "b", CreatedAt: when, Reply: &commentref.Link{UID: "a"}},
		}, []string{"a:0", "b:1", "c:1", "d:1"}},
		{"unknown parent and cycle", []CommentEntry{{UID: "a", Reply: &commentref.Link{UID: "b"}}, {UID: "b", Reply: &commentref.Link{UID: "a"}}, {UID: "c", Reply: &commentref.Link{UID: "unknown"}}}, []string{"c:0", "a:0", "b:1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var rows []string
			for _, c := range threadOrder(tt.comments) {
				rows = append(rows, fmt.Sprintf("%s:%d", c.UID, c.depth))
				for _, leaf := range c.leaves {
					rows = append(rows, fmt.Sprintf("%s:%d", leaf.link.UID, leaf.depth))
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

func TestCommentTags(t *testing.T) {
	for _, tt := range []struct {
		name      string
		links     []commentref.Link
		truncated bool
		want      string
	}{
		{"confirm", []commentref.Link{{Kind: "confirm"}}, false, "confirm"},
		{"both", []commentref.Link{{Kind: "refute"}, {Kind: "confirm"}}, false, "confirm · refute"},
		{"stale", []commentref.Link{{Kind: "confirm", TargetEdited: true}, {Kind: "refute", TargetEdited: true}}, false, "confirm (before edit) · refute (before edit)"},
		{"mixed", []commentref.Link{{Kind: "confirm", TargetEdited: true}, {Kind: "confirm"}}, false, "confirm"},
		{"truncated stale", []commentref.Link{{Kind: "confirm", TargetEdited: true}}, true, "confirm (before edit)"},
		{"other kinds", []commentref.Link{{Kind: "reply"}, {Kind: "supersede"}}, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, stripANSI(commentTags(CommentEntry{Backlinks: tt.links, BacklinksTruncated: tt.truncated})))
		})
	}
}

func TestCommentTreeCrossIssueParent(t *testing.T) {
	projected := commentref.Project([]commentref.Record{
		{UID: "a", IssueUID: "local"},
		{UID: "b", ReplyToUID: "a", ReplyKind: "confirm", IssueUID: "foreign"},
		{UID: "c", ReplyToUID: "b", ReplyKind: "refute", IssueUID: "local"},
	}, nil)
	comments := threadOrder([]CommentEntry{
		{UID: "c", Handle: "c:child", Reply: projected[2].Reply},
		{UID: "a", Handle: "c:root", Backlinks: projected[0].Backlinks},
	})
	require.Equal(t, "a", comments[0].UID)
	require.Equal(t, "b", comments[0].leaves[0].link.UID)
	require.Equal(t, 1, comments[0].leaves[0].depth)
	require.Equal(t, "c", comments[1].UID)
	require.Equal(t, 2, comments[1].depth)
	chunks := commentChunks(comments, 80, -1, tabState{})
	require.Contains(t, stripANSI(chunks[1].lines[0]), "      c:child")
}

func TestCommentTreeForeignTargetTags(t *testing.T) {
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	edited := when.Add(time.Minute)
	for _, freshConfirm := range []bool{false, true} {
		t.Run(fmt.Sprint("fresh confirm=", freshConfirm), func(t *testing.T) {
			confirmTime := when
			if freshConfirm {
				confirmTime = edited.Add(time.Minute)
			}
			projected := commentref.Project([]commentref.Record{
				{UID: "a", IssueUID: "local"},
				{UID: "b", Handle: "c:foreign", ReplyToUID: "a", ReplyKind: "reply", IssueUID: "foreign", EditedAt: &edited},
				{UID: "c", ReplyToUID: "b", ReplyKind: "confirm", IssueUID: "local", CreatedAt: confirmTime},
				{UID: "d", ReplyToUID: "b", ReplyKind: "refute", IssueUID: "local", CreatedAt: edited.Add(time.Minute)},
			}, nil)
			comments := threadOrder([]CommentEntry{
				{UID: "a", Backlinks: projected[0].Backlinks},
				{UID: "c", Reply: projected[2].Reply},
				{UID: "d", Reply: projected[3].Reply},
			})
			chunks := commentChunks(comments, 80, -1, tabState{})
			want := "confirm (before edit) · refute"
			if freshConfirm {
				want = "confirm · refute"
			}
			require.Contains(t, stripANSI(strings.Join(chunks[0].lines, "\n")), want)
		})
	}
}

func TestCommentTagsProjectedTruncation(t *testing.T) {
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	edited := when.Add(time.Hour)
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprint("fresh=", fresh), func(t *testing.T) {
			records := []commentref.Record{{UID: "target", EditedAt: &edited}}
			for i := range 60 {
				records = append(records, commentref.Record{UID: fmt.Sprintf("reply-%02d", i), ReplyToUID: "target", ReplyKind: "confirm", CreatedAt: when.Add(time.Duration(i) * time.Minute)})
			}
			want := "confirm (before edit)"
			if fresh {
				records = append(records, commentref.Record{UID: "fresh", ReplyToUID: "target", ReplyKind: "confirm", CreatedAt: edited.Add(time.Minute)})
				want = "confirm"
			}
			target := commentref.Project(records, nil)[0]
			require.True(t, target.BacklinksTruncated)
			require.Len(t, target.Backlinks, 50)
			require.Equal(t, want, stripANSI(commentTags(CommentEntry{Backlinks: target.Backlinks, BacklinksTruncated: target.BacklinksTruncated})))
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
	require.Contains(t, commentTags(c), "\x1b[")
	for width := 30; width <= 79; width++ {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			chunks := commentChunks([]CommentEntry{c}, width, -1, tabState{})
			plain := stripANSI(strings.Join(chunks[0].lines[1:], "\n"))
			require.NotContains(t, plain, "\x1b")
			require.Contains(t, strings.Join(strings.Fields(plain), ""), "confirm(beforeedit)·refute(beforeedit)")
		})
	}
}

func TestCommentTreeNarrowIndent(t *testing.T) {
	defer snapshotInit(t)()
	comments := []CommentEntry{{UID: "a", Handle: "c:a", Body: "Read the evidence.", depth: 4,
		leaves: []commentLeaf{{link: commentref.Link{Handle: "b:reply", Kind: "confirm"}, depth: 4}},
	}}
	chunks := commentChunks(comments, 20, -1, tabState{})
	require.Contains(t, stripANSI(strings.Join(chunks[0].lines, "\n")), "  Read the evidence.")
	assertGolden(t, "detail-comment-tree-tiny", strings.Join(chunks[0].lines, "\n"))
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
	dm.comments = []CommentEntry{{UID: "0", Handle: "c:0", Author: "worker", Body: "Start.", CreatedAt: when}}
	for i := 1; i < 7; i++ {
		dm.comments = append(dm.comments, CommentEntry{UID: fmt.Sprint(i), Handle: fmt.Sprintf("c:%d", i), Author: "worker", Body: "Evidence remains visible at depth.", CreatedAt: when.Add(time.Duration(i) * time.Minute), Reply: &commentref.Link{UID: fmt.Sprint(i - 1), Handle: fmt.Sprintf("c:%d", i-1), Kind: "confirm"}})
	}
	dm.comments[5].EditedAt = &edited
	dm.comments = threadOrder(dm.comments)
	dm.tabCursor = 6
	assertGolden(t, "detail-comment-tree-narrow", dm.View(50, 60, viewChrome{}))
}

func TestCommentTreeMovedTarget(t *testing.T) {
	target := commentref.Record{UID: "target", Handle: "c:target", IssueUID: "other-issue", IssueShortID: "bbbb", ProjectName: "other-project"}
	projected := commentref.Project([]commentref.Record{
		{UID: "reply", IssueUID: "local", ReplyToUID: "target", ReplyKind: "confirm"},
	}, map[string]commentref.TargetState{"target": {Status: "moved", Target: &target}})
	chunks := commentChunks(threadOrder([]CommentEntry{{UID: "reply", Reply: projected[0].Reply}}), 80, -1, tabState{})
	require.Contains(t, stripANSI(strings.Join(chunks[0].lines, "\n")), "↳ Confirms other-project#bbbb:target (moved)")
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
	m, _ = updateModel(m, runeKey(']'))
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	jump := cmd().(jumpDetailMsg)
	require.Equal(t, "foreign", jump.commentUID)
	m, _ = updateModel(m, jump)
	m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, issue: &Issue{UID: "issue-b", ShortID: "bbbb"}})
	m, _ = updateModel(m, commentsFetchedMsg{gen: m.detail.gen, comments: []CommentEntry{{UID: "foreign"}}})
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, "issue-a", m.detail.issue.UID)
	require.Equal(t, "a", m.detail.comments[m.detail.tabCursor].UID)
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
			dm = dm.applyFetched(commentsFetchedMsg{gen: 1, err: errors.New("refresh failed")})
			require.Equal(t, 2, dm.tabCursor)
			dm = dm.applyFetched(commentsFetchedMsg{gen: 1, comments: []CommentEntry{{UID: "a"}, {UID: "reply", Reply: &commentref.Link{UID: "a"}}, {UID: "b"}}})
			require.Equal(t, "b", dm.comments[dm.tabCursor].UID)
		})
	}
}
