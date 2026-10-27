package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/mattn/go-runewidth"

	"go.kenn.io/kata/internal/commentref"
)

func TestThreadOrderKeepsRepliesWithTheirParents(t *testing.T) {
	comments := []CommentEntry{
		{UID: "a"},
		{UID: "b"},
		{UID: "c", Reply: &commentref.Link{UID: "a"}},
		{UID: "d", Reply: &commentref.Link{UID: "c"}},
		{UID: "e", Reply: &commentref.Link{UID: "a"}},
	}
	got := threadOrder(comments)
	ids := make([]string, len(got))
	for i, c := range got {
		ids[i] = c.UID
	}
	if want := []string{"a", "c", "d", "e", "b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}

func TestThreadOrderRetainsMalformedCycle(t *testing.T) {
	comments := []CommentEntry{
		{UID: "a", Reply: &commentref.Link{UID: "b"}},
		{UID: "b", Reply: &commentref.Link{UID: "a"}},
		{UID: "c", Reply: &commentref.Link{UID: "foreign"}},
	}
	got := threadOrder(comments)
	ids := make([]string, len(got))
	for i, c := range got {
		ids[i] = c.UID
	}
	if want := []string{"c", "a", "b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}

func TestCommentRefreshPreservesSelectedUID(t *testing.T) {
	dm := detailModel{activeTab: tabComments, detailFocus: focusActivity, gen: 1, comments: []CommentEntry{{UID: "a"}, {UID: "b"}}, tabCursor: 1}
	dm = dm.applyFetched(commentsFetchedMsg{gen: 1, comments: []CommentEntry{
		{UID: "a"}, {UID: "b"}, {UID: "reply", Reply: &commentref.Link{UID: "a"}},
	}})
	if got := dm.comments[dm.tabCursor].UID; got != "b" {
		t.Fatalf("selected %q, want b", got)
	}
}

func drawCommentForest(t *rapid.T) []CommentEntry {
	n := rapid.IntRange(1, 30).Draw(t, "count")
	comments := make([]CommentEntry, n)
	for i := range comments {
		comments[i].UID = fmt.Sprintf("c%d", i)
		parent := rapid.IntRange(-1, i-1).Draw(t, fmt.Sprintf("parent-%d", i))
		if parent >= 0 {
			comments[i].Reply = &commentref.Link{UID: comments[parent].UID}
		}
	}
	for i := len(comments) - 1; i > 0; i-- {
		j := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("shuffle-%d", i))
		comments[i], comments[j] = comments[j], comments[i]
	}
	return comments
}

func TestThreadOrderForestPermutation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := drawCommentForest(t)
		got := threadOrder(input)
		if len(got) != len(input) {
			t.Fatalf("got %d comments, want %d", len(got), len(input))
		}
		seen := make(map[string]bool)
		for _, c := range got {
			if seen[c.UID] {
				t.Fatalf("duplicate %s", c.UID)
			}
			seen[c.UID] = true
		}
		for _, c := range input {
			if !seen[c.UID] {
				t.Fatalf("missing %s", c.UID)
			}
		}
	})
}

func TestThreadOrderForestSubtrees(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := drawCommentForest(t)
		got := threadOrder(input)
		parents := make(map[string]string)
		for _, c := range input {
			if c.Reply != nil {
				parents[c.UID] = c.Reply.UID
			}
		}
		positions := make(map[string]int)
		for i, c := range got {
			positions[c.UID] = i
		}
		for _, root := range input {
			first, last, count := positions[root.UID], positions[root.UID], 0
			for _, c := range input {
				for id := c.UID; id != ""; id = parents[id] {
					if id == root.UID {
						pos := positions[c.UID]
						if pos < first {
							t.Fatalf("child before parent %s", root.UID)
						}
						if pos > last {
							last = pos
						}
						count++
						break
					}
				}
			}
			if last-first+1 != count {
				t.Fatalf("subtree of %s is not contiguous", root.UID)
			}
		}

	})
}

func TestThreadOrderForestSiblingOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := drawCommentForest(t)
		got := threadOrder(input)
		positions := make(map[string]int)
		parents := make(map[string]string)
		for i, c := range got {
			positions[c.UID] = i
		}
		for _, c := range input {
			if c.Reply != nil {
				parents[c.UID] = c.Reply.UID
			}
		}
		lastByParent := make(map[string]int)
		for _, c := range input {
			parent := parents[c.UID]
			if previous, ok := lastByParent[parent]; ok && previous > positions[c.UID] {
				t.Fatalf("siblings of %s changed order", parent)
			}
			lastByParent[parent] = positions[c.UID]
		}
	})
}

func TestThreadOrderForestIdempotence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		got := threadOrder(drawCommentForest(t))
		if !reflect.DeepEqual(threadOrder(got), got) {
			t.Fatal("ordering is not idempotent")
		}
	})
}

func TestThreadOrderCyclesRetainEveryComment(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 30).Draw(t, "cycle size")
		input := make([]CommentEntry, n)
		for i := range input {
			input[i] = CommentEntry{UID: fmt.Sprintf("c%d", i), Reply: &commentref.Link{UID: fmt.Sprintf("c%d", (i+1)%n)}}
		}
		got := threadOrder(input)
		if len(got) != n {
			t.Fatalf("got %d comments, want %d", len(got), n)
		}
		seen := make(map[string]bool)
		for _, c := range got {
			if seen[c.UID] {
				t.Fatalf("duplicate %s", c.UID)
			}
			seen[c.UID] = true
		}
		if !reflect.DeepEqual(threadOrder(got), got) {
			t.Fatal("cycle ordering is not idempotent")
		}
	})
}

func TestCommentTreeShowsIndependentFlagTags(t *testing.T) {
	old := activeColorMode
	activeColorMode = colorNone
	t.Cleanup(func() { activeColorMode = old })
	tests := []struct {
		name  string
		links []commentref.Link
		want  string
	}{
		{"confirm", []commentref.Link{{UID: "foreign", Kind: "confirm"}}, "+ confirm"},
		{"refute", []commentref.Link{{UID: "foreign", Kind: "refute"}}, "x refute"},
		{"both", []commentref.Link{{UID: "foreign-a", Kind: "confirm", TargetEdited: true}, {UID: "foreign-b", Kind: "refute"}}, "+ confirm (before edit) x refute"},
		{"both stale", []commentref.Link{{UID: "foreign-a", Kind: "confirm", TargetEdited: true}, {UID: "foreign-b", Kind: "refute", TargetEdited: true}}, "+ confirm (before edit) x refute (before edit)"},
		{"fresh confirm", []commentref.Link{{UID: "foreign-a", Kind: "confirm", TargetEdited: true}, {UID: "foreign-b", Kind: "confirm"}}, "+ confirm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := commentChunks([]CommentEntry{{UID: "a", Handle: "c:a", Author: "worker", Backlinks: tt.links}}, 96, -1, tabState{})
			header := stripANSI(chunks[0].lines[0])
			if !strings.Contains(header, tt.want) {
				t.Fatalf("header %q does not contain %q", header, tt.want)
			}
			if tt.name == "fresh confirm" && strings.Contains(header, "before edit") {
				t.Fatalf("fresh confirmation incorrectly marked stale: %q", header)
			}
		})
	}
}

func TestCommentTreeRendersChildWithFlagAndRail(t *testing.T) {
	old := activeColorMode
	activeColorMode = colorNone
	t.Cleanup(func() { activeColorMode = old })
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a", Author: "worker", Backlinks: []commentref.Link{{UID: "b", Kind: "refute"}}},
		{UID: "b", Handle: "c:b", Author: "reviewer", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "refute"}},
	}
	chunks := commentChunks(comments, 80, -1, tabState{})
	header := stripANSI(chunks[1].lines[0])
	if !strings.Contains(header, `\-x c:b  refute c:a`) {
		t.Fatalf("child header %q lacks rail and target", header)
	}
}

func TestCommentTreeHeaderWrapsWithoutRepeatingHandle(t *testing.T) {
	old := activeColorMode
	activeColorMode = colorNone
	t.Cleanup(func() { activeColorMode = old })
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a", Backlinks: []commentref.Link{{UID: "b", Kind: "confirm"}, {UID: "remote", Kind: "refute", TargetEdited: true}}},
		{UID: "b", Handle: "c:b", Author: "reviewer", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "confirm"}},
	}
	chunks := commentChunks(comments, 32, -1, tabState{})
	for _, chunk := range chunks {
		for _, line := range chunk.lines {
			if runewidth.StringWidth(stripANSI(line)) > 32 {
				t.Fatalf("line exceeds width: %q", line)
			}
		}
	}
	header := stripANSI(chunks[1].lines[0])
	if strings.Count(header, "c:b") != 1 {
		t.Fatalf("child header repeats handle: %q", header)
	}
}

func TestCommentTreeNestedRailsAndLocalEvidence(t *testing.T) {
	old := activeColorMode
	activeColorMode = colorNone
	t.Cleanup(func() { activeColorMode = old })
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a", Backlinks: []commentref.Link{{UID: "b", IssueUID: "issue-a", Handle: "c:b", Kind: "refute", Body: "local evidence"}}},
		{UID: "b", Handle: "c:b", Body: "local evidence", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "refute"}},
		{UID: "c", Handle: "c:c", Reply: &commentref.Link{UID: "b", Handle: "c:b", Kind: "confirm"}},
		{UID: "d", Handle: "c:d", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "reply"}},
	}
	chunks := commentChunks(comments, 80, 0, tabState{})
	var lines []string
	for _, chunk := range chunks {
		lines = append(lines, chunk.lines...)
	}
	out := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(out, "|   \\-+ c:c  confirm c:b") {
		t.Fatalf("missing nested rail in:\n%s", out)
	}
	if strings.Count(out, "local evidence") != 1 || !strings.Contains(out, "(shown in thread)") {
		t.Fatalf("local evidence duplicated or missing marker:\n%s", out)
	}
}

func TestCommentTreeShowsForeignLeafAndEvidence(t *testing.T) {
	old := activeColorMode
	activeColorMode = colorNone
	t.Cleanup(func() { activeColorMode = old })
	comments := []CommentEntry{{UID: "a", Handle: "c:a", Backlinks: []commentref.Link{
		{UID: "foreign", IssueUID: "issue-b", Handle: "bbbb:leaf", Kind: "refute", Body: "foreign evidence"},
	}}}
	chunks := commentChunks(comments, 80, 0, tabState{})
	out := stripANSI(strings.Join(chunks[0].lines, "\n"))
	if !strings.Contains(out, `\-x bbbb:leaf  refute c:a`) || !strings.Contains(out, "foreign evidence") {
		t.Fatalf("missing foreign leaf or evidence:\n%s", out)
	}
}

func TestCommentTreeFitsWidths(t *testing.T) {
	old := activeColorMode
	activeColorMode = colorNone
	t.Cleanup(func() { activeColorMode = old })
	comments := []CommentEntry{
		{UID: "a", Handle: "c:root", Author: "worker", Backlinks: []commentref.Link{{UID: "b", Kind: "refute", TargetEdited: true}}},
		{UID: "b", Handle: "c:child", Author: "reviewer", Body: "A longer body to wrap safely.", Reply: &commentref.Link{UID: "a", Handle: "c:root", Kind: "refute"}},
	}
	for width := 24; width <= 120; width++ {
		for _, chunk := range commentChunks(comments, width, -1, tabState{}) {
			for _, line := range chunk.lines {
				if runewidth.StringWidth(stripANSI(line)) > width {
					t.Fatalf("width %d: line overflows: %q", width, line)
				}
			}
		}
	}
}

func TestSnapshot_CommentTree(t *testing.T) {
	defer snapshotInit(t)()
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	dm := snapDetailFixture()
	dm.issue.Author = "worker"
	dm.issue.Title = "Review a finding"
	dm.issue.Body = "Discuss the evidence."
	dm.activeTab = tabComments
	dm.tabCursor = 0
	dm.comments = []CommentEntry{
		{UID: "a", Handle: "c:root", Author: "worker-a", Body: "A finding to inspect.", CreatedAt: when, Backlinks: []commentref.Link{{UID: "b", IssueUID: "issue-a", Handle: "c:reply", Kind: "refute", Body: "Local evidence."}, {UID: "foreign", IssueUID: "issue-b", Handle: "bbbb:reply", Kind: "confirm", Author: "worker-b", Body: "Foreign evidence."}}},
		{UID: "b", Handle: "c:reply", Author: "worker-c", Body: "Local evidence.", CreatedAt: when.Add(time.Minute), Reply: &commentref.Link{UID: "a", IssueUID: "issue-a", Handle: "c:root", Kind: "refute"}, Backlinks: []commentref.Link{{UID: "c", IssueUID: "issue-a", Handle: "c:nested", Kind: "confirm"}}},
		{UID: "c", Handle: "c:nested", Author: "worker-d", Body: "Confirmed.", CreatedAt: when.Add(2 * time.Minute), Reply: &commentref.Link{UID: "b", IssueUID: "issue-a", Handle: "c:reply", Kind: "confirm"}},
		{UID: "d", Handle: "c:other", Author: "worker-e", Body: "Another reply.", CreatedAt: when.Add(3 * time.Minute), Reply: &commentref.Link{UID: "a", IssueUID: "issue-a", Handle: "c:root", Kind: "reply"}},
		{UID: "e", Handle: "c:plain", Author: "worker-f", Body: "Another finding.", CreatedAt: when.Add(4 * time.Minute)},
	}
	assertGolden(t, "detail-comment-tree", dm.View(120, 60, viewChrome{}))
	assertGolden(t, "detail-comment-tree-narrow", dm.View(50, 60, viewChrome{}))
	t.Setenv("KATA_COLOR_MODE", "dark")
	applyDefaultColorMode()
	assertGolden(t, "detail-comment-tree-unicode", dm.View(80, 60, viewChrome{}))
	t.Setenv("KATA_COLOR_MODE", "none")
	applyDefaultColorMode()
}

func TestCommentTreeWrapsHeaderAtFields(t *testing.T) {
	old := activeColorMode
	activeColorMode = colorNone
	t.Cleanup(func() { activeColorMode = old })
	comments := []CommentEntry{
		{UID: "a", Handle: "c:root"},
		{UID: "b", Handle: "c:child", Author: "reviewer", CreatedAt: time.Date(2026, 4, 30, 10, 1, 0, 0, time.UTC), Reply: &commentref.Link{UID: "a", Handle: "c:root", Kind: "refute"}},
	}
	chunks := commentChunks(comments, 44, -1, tabState{})
	out := stripANSI(strings.Join(chunks[1].lines, "\n"))
	if !strings.Contains(out, "Apr 30 10:01") {
		t.Fatalf("timestamp split across lines:\n%s", out)
	}
}

func TestCommentTreeReplyAndSupersedeHaveNoTags(t *testing.T) {
	for _, kind := range []string{"reply", "supersede"} {
		c := CommentEntry{UID: "a", Backlinks: []commentref.Link{{UID: "b", Kind: kind}}}
		if got := commentFlagTags(c); got != "" {
			t.Fatalf("%s produced tag %q", kind, got)
		}
	}
}

func TestCommentTreeCursorDrivesReplyForm(t *testing.T) {
	m := newTestModel()
	m.view = viewDetail
	m.detail = detailModel{issue: &Issue{UID: "issue-a", Status: "open"}, activeTab: tabComments, detailFocus: focusActivity, gen: 1}
	m, _ = updateModel(m, commentsFetchedMsg{gen: 1, comments: []CommentEntry{
		{UID: "a"}, {UID: "root-b"}, {UID: "b", Reply: &commentref.Link{UID: "a"}}, {UID: "c", Reply: &commentref.Link{UID: "b"}},
	}})
	m, _ = updateModel(m, runeKey('j'))
	m, _ = updateModel(m, runeKey('j'))
	m, _ = updateModel(m, runeKey('R'))
	if got := m.input.target.replyUID; got != "c" {
		t.Fatalf("reply target = %q, want c", got)
	}
}

func TestCommentTreeNarrowForeignTargetFits(t *testing.T) {
	comments := []CommentEntry{{UID: "a", Handle: "c:root", Author: "worker", Reply: &commentref.Link{UID: "other", Handle: "bbbb:longtarget", Kind: "supersede", Author: "reviewer", Status: "moved", TargetEdited: true}}}
	for width := 24; width <= 50; width++ {
		for _, line := range commentChunks(comments, width, -1, tabState{})[0].lines {
			if runewidth.StringWidth(stripANSI(line)) > width {
				t.Fatalf("width %d: %q", width, line)
			}
		}
	}
}

func TestCommentTreeShowsEditedTargetOnLocalChild(t *testing.T) {
	comments := []CommentEntry{{UID: "a"}, {UID: "b", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "confirm", TargetEdited: true}}}
	chunks := commentChunks(comments, 80, -1, tabState{})
	if out := stripANSI(strings.Join(chunks[1].lines, "\n")); !strings.Contains(out, "Target edited after this reply") {
		t.Fatalf("missing edit notice: %s", out)
	}
}

func TestCommentTreeDeepRowsStayWithinWidth(t *testing.T) {
	comments := make([]CommentEntry, 10)
	for i := range comments {
		comments[i] = CommentEntry{UID: fmt.Sprintf("id-%d", i), Handle: fmt.Sprintf("c:node-%d", i), Author: "reviewer", Body: "Evidence remains visible at depth."}
		if i > 0 {
			comments[i].Reply = &commentref.Link{UID: fmt.Sprintf("id-%d", i-1), Handle: fmt.Sprintf("c:node-%d", i-1), Kind: "reply"}
		}
	}
	for _, chunk := range commentChunks(comments, 44, -1, tabState{}) {
		for _, line := range chunk.lines {
			if runewidth.StringWidth(stripANSI(line)) > 44 {
				t.Fatalf("line exceeds width: %q", line)
			}
		}
	}
	last := stripANSI(strings.Join(commentChunks(comments, 44, -1, tabState{})[9].lines, "\n"))
	if !strings.Contains(last, "reply c:node-8") {
		t.Fatalf("missing real parent: %s", last)
	}
}

func TestCommentRefreshClampsWhenSelectionDisappears(t *testing.T) {
	dm := detailModel{activeTab: tabComments, detailFocus: focusActivity, gen: 1, comments: []CommentEntry{{UID: "a"}, {UID: "b"}}, tabCursor: 1}
	dm = dm.applyFetched(commentsFetchedMsg{gen: 1, comments: []CommentEntry{{UID: "a"}}})
	if dm.tabCursor != 0 {
		t.Fatalf("cursor = %d, want 0", dm.tabCursor)
	}
}

func TestCommentTreeKeepsTagStyleWhenWrapped(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	applyColorMode(colorDark, true)
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	comments := []CommentEntry{{UID: "a", Handle: "c:root", Author: "reviewer-with-a-long-name", Backlinks: []commentref.Link{{UID: "b", Kind: "confirm"}}}}
	lines := commentChunks(comments, 32, -1, tabState{})[0].lines
	out := strings.Join(lines, "\n")
	if want := openStyle.Render("✓ confirm"); !strings.Contains(out, want) {
		t.Fatalf("wrapped tag lost style: %q", out)
	}
}

func TestCommentTreeStylesRailsAndKindGlyph(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	applyColorMode(colorDark, true)
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	comments := []CommentEntry{{UID: "a", Handle: "c:a"}, {UID: "b", Handle: "c:b", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "refute"}}}
	header := commentChunks(comments, 80, -1, tabState{})[1].lines[0]
	if !strings.Contains(header, subtleStyle.Render("└─")) || !strings.Contains(header, errorStyle.Render("✗")) {
		t.Fatalf("rail or kind glyph lost style: %q", header)
	}
}

func TestCommentTreeLongFieldsFitNarrowWidth(t *testing.T) {
	comments := []CommentEntry{{UID: "a", Handle: "c:abcdefghijklmnopqrstuvwxyz", Reply: &commentref.Link{UID: "foreign", Handle: "bbbb:abcdefghijklmnopqrstuvwxyz", Kind: "reply"}}}
	for _, chunk := range commentChunks(comments, 24, -1, tabState{}) {
		for _, line := range chunk.lines {
			if runewidth.StringWidth(stripANSI(line)) > 24 {
				t.Fatalf("line exceeds width: %q", line)
			}
		}
	}
}

func TestCommentTreeDeepStaleTagsFitNarrowWidth(t *testing.T) {
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a"},
		{UID: "b", Handle: "c:b", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "reply"}, Backlinks: []commentref.Link{{UID: "foreign", Kind: "confirm", TargetEdited: true}, {UID: "foreign-2", Kind: "refute", TargetEdited: true}}},
	}
	for _, chunk := range commentChunks(comments, 24, -1, tabState{}) {
		for _, line := range chunk.lines {
			if runewidth.StringWidth(stripANSI(line)) > 24 {
				t.Fatalf("line exceeds width: %q", line)
			}
		}
	}
}

func TestCommentTreePreservesFullAttribution(t *testing.T) {
	comments := []CommentEntry{{UID: "a", Handle: "c:a", Author: "worker-with-a-long-name", Backlinks: []commentref.Link{{UID: "foreign", Kind: "reply"}}}}
	out := stripANSI(strings.Join(commentChunks(comments, 80, -1, tabState{})[0].lines, "\n"))
	if !strings.Contains(out, "worker-with-a-long-name") {
		t.Fatalf("attribution truncated:\n%s", out)
	}
}

func TestCommentTreeWrappedBodyRetainsFollowingRows(t *testing.T) {
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a"},
		{UID: "b", Handle: "c:b", Body: strings.Repeat("longword ", 12) + "\n\nDistinct follow-up.", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "reply"}, Backlinks: []commentref.Link{{UID: "foreign", Kind: "refute"}}},
	}
	out := stripANSI(strings.Join(commentChunks(comments, 32, -1, tabState{})[1].lines, "\n"))
	if strings.Count(out, "Distinct follow-up.") != 1 || !strings.Contains(out, "Refutations 1") {
		t.Fatalf("lost or duplicated body/summary:\n%s", out)
	}
}

func TestCommentTreeNestedWrappedTagKeepsStyle(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	applyColorMode(colorDark, true)
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a"},
		{UID: "b", Handle: "c:b", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "reply"}, Backlinks: []commentref.Link{{UID: "foreign", Kind: "confirm", TargetEdited: true}}},
	}
	out := strings.Join(commentChunks(comments, 28, -1, tabState{})[1].lines, "\n")
	colorPrefix, _, _ := strings.Cut(openStyle.Render("✓"), "✓")
	if !strings.Contains(out, colorPrefix+"✓ confirm") || strings.Count(stripANSI(out), ")") != 1 {
		t.Fatalf("nested tag lost style or duplicated text: %q", out)
	}
}

func TestCommentTreeStaleConfirmFitsMinimumWidth(t *testing.T) {
	c := CommentEntry{UID: "a", Handle: "c:a", Backlinks: []commentref.Link{{UID: "foreign", Kind: "confirm", TargetEdited: true}}}
	for _, line := range commentChunks([]CommentEntry{c}, 24, -1, tabState{})[0].lines {
		if runewidth.StringWidth(stripANSI(line)) > 24 {
			t.Fatalf("line exceeds width: %q", line)
		}
	}
}

func TestCommentTreeWrappedNestedHeaderKeepsDepth(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	applyColorMode(colorNone, false)
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	comments := []CommentEntry{
		{UID: "a", Handle: "c:a"},
		{UID: "b", Handle: "c:b", Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "reply"}},
		{UID: "c", Handle: "c:c", Author: "worker-with-long-attribution", Reply: &commentref.Link{UID: "b", Handle: "c:b", Kind: "confirm"}},
	}
	header := stripANSI(commentChunks(comments, 44, -1, tabState{})[2].lines[0])
	if !strings.HasPrefix(header, `      \-+`) {
		t.Fatalf("nested header lost depth: %q", header)
	}
}

func TestCommentFlagTagsReflectDirectKindsAndFreshness(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	rapid.Check(t, func(t *rapid.T) {
		confirm := rapid.Bool().Draw(t, "confirm")
		refute := rapid.Bool().Draw(t, "refute")
		staleConfirm := rapid.Bool().Draw(t, "stale confirm")
		staleRefute := rapid.Bool().Draw(t, "stale refute")
		var links []commentref.Link
		if confirm {
			links = append(links, commentref.Link{Kind: "confirm", TargetEdited: staleConfirm})
		}
		if refute {
			links = append(links, commentref.Link{Kind: "refute", TargetEdited: staleRefute})
		}
		for _, mode := range []colorMode{colorNone, colorDark} {
			applyColorMode(mode, true)
			confirmGlyph, refuteGlyph := "+", "x"
			if mode == colorDark {
				confirmGlyph, refuteGlyph = "✓", "✗"
			}
			var expected []string
			if confirm {
				text := confirmGlyph + " confirm"
				if staleConfirm {
					text += " (before edit)"
				}
				expected = append(expected, text)
			}
			if refute {
				text := refuteGlyph + " refute"
				if staleRefute {
					text += " (before edit)"
				}
				expected = append(expected, text)
			}
			if got, want := stripANSI(commentFlagTags(CommentEntry{Backlinks: links})), strings.Join(expected, " "); got != want {
				t.Fatalf("tags = %q, want %q", got, want)
			}
		}
	})
}

func TestCommentFlagTagsIgnoreOrderDuplicatesAndOtherKinds(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	rapid.Check(t, func(t *rapid.T) {
		links := []commentref.Link{{Kind: "confirm", TargetEdited: rapid.Bool().Draw(t, "confirm stale")}, {Kind: "refute", TargetEdited: rapid.Bool().Draw(t, "refute stale")}}
		for _, mode := range []colorMode{colorNone, colorDark} {
			applyColorMode(mode, true)
			original := commentFlagTags(CommentEntry{Backlinks: links})
			changed := []commentref.Link{links[1], {Kind: "reply"}, links[0], links[0], {Kind: "supersede"}}
			got := commentFlagTags(CommentEntry{Backlinks: changed, BacklinksTruncated: true})
			if got != original {
				t.Fatalf("unrelated changes affected tags: %q != %q", got, original)
			}
		}
	})
}

func TestCommentTreeForeignLeafFitsAndKeepsGlyphStyle(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	applyColorMode(colorDark, true)
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	c := CommentEntry{UID: "a", Handle: "c:root", Backlinks: []commentref.Link{{UID: "foreign", Handle: "bbbb:foreign", Kind: "refute", Author: "worker-with-long-name"}}}
	lines := commentChunks([]CommentEntry{c}, 24, -1, tabState{})[0].lines
	out := strings.Join(lines, "\n")
	for _, line := range lines {
		if runewidth.StringWidth(stripANSI(line)) > 24 {
			t.Fatalf("line exceeds width: %q", line)
		}
	}
	if !strings.Contains(out, errorStyle.Render("✗")) {
		t.Fatalf("foreign glyph lost style: %q", out)
	}
}

func TestCommentTreeForeignLeafWrapsWithoutBreakingANSI(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	applyColorMode(colorDark, true)
	t.Cleanup(func() { applyColorMode(oldMode, oldDark) })
	c := CommentEntry{UID: "a", Handle: "c:root", Backlinks: []commentref.Link{{UID: "foreign", Handle: "bbbb:foreign", Kind: "refute", Author: "worker"}}}
	lines := commentChunks([]CommentEntry{c}, 40, -1, tabState{})[0].lines
	var leaf string
	for _, line := range lines {
		if strings.Contains(stripANSI(line), "bbbb:foreign") {
			leaf = stripANSI(line)
			break
		}
	}
	if !strings.Contains(leaf, "refute c:root") {
		t.Fatalf("leaf wrapped despite fitting: %q", leaf)
	}
	for _, line := range commentChunks([]CommentEntry{c}, 16, -1, tabState{})[0].lines {
		if strings.Contains(stripANSI(line), "\x1b") {
			t.Fatalf("incomplete ANSI sequence: %q", line)
		}
	}
}

func TestCommentTreeDeepChainAllocationsScaleWithRows(t *testing.T) {
	chain := func(n int) []CommentEntry {
		comments := make([]CommentEntry, n)
		for i := range comments {
			comments[i] = CommentEntry{UID: fmt.Sprintf("id-%d", i), Handle: fmt.Sprintf("c:%d", i)}
			if i > 0 {
				comments[i].Reply = &commentref.Link{UID: fmt.Sprintf("id-%d", i-1), Handle: fmt.Sprintf("c:%d", i-1), Kind: "reply"}
			}
		}
		return comments
	}
	small, large := chain(100), chain(300)
	smallAllocs := testing.AllocsPerRun(1, func() { commentChunks(small, 44, -1, tabState{}) })
	largeAllocs := testing.AllocsPerRun(1, func() { commentChunks(large, 44, -1, tabState{}) })
	if largeAllocs > 5*smallAllocs {
		t.Fatalf("allocations grew superlinearly: 100 rows %.0f, 300 rows %.0f", smallAllocs, largeAllocs)
	}
}

func TestCommentRefreshKeepsVisibleSelectionInView(t *testing.T) {
	dm := detailModel{issue: &Issue{UID: "issue-a", Status: "open"}, activeTab: tabComments, detailFocus: focusActivity, gen: 1, lastTermWidth: 80, lastTermHeight: 24, comments: []CommentEntry{{UID: "a", Handle: "c:a"}, {UID: "b", Handle: "c:b"}}, tabCursor: 1}
	dm = dm.revealCursor()
	before := stripANSI(dm.View(80, 24, viewChrome{}))
	if !strings.Contains(before, "> c:b") {
		t.Fatalf("selection not visible before refresh:\n%s", before)
	}
	dm = dm.applyFetched(commentsFetchedMsg{gen: 1, comments: []CommentEntry{
		{UID: "a", Handle: "c:a"}, {UID: "b", Handle: "c:b"}, {UID: "reply", Handle: "c:reply", Body: strings.Repeat("long reply line\n", 60), Reply: &commentref.Link{UID: "a", Handle: "c:a", Kind: "reply"}},
	}})
	after := stripANSI(dm.View(80, 24, viewChrome{}))
	if !strings.Contains(after, "> c:b") {
		t.Fatalf("selection lost after refresh:\n%s", after)
	}
}

func TestCommentRefreshDoesNotInterruptManualScroll(t *testing.T) {
	dm := detailModel{issue: &Issue{UID: "issue-a", Status: "open", Body: strings.Repeat("description line\n", 50)}, activeTab: tabComments, detailFocus: focusActivity, gen: 1, lastTermWidth: 80, lastTermHeight: 24, comments: []CommentEntry{{UID: "a", Handle: "c:a"}}, tabCursor: 0}
	before := stripANSI(dm.View(80, 24, viewChrome{}))
	if strings.Contains(before, "> c:a") {
		t.Fatal("selection unexpectedly visible before refresh")
	}
	dm = dm.applyFetched(commentsFetchedMsg{gen: 1, comments: []CommentEntry{{UID: "a", Handle: "c:a"}, {UID: "b", Reply: &commentref.Link{UID: "a", Kind: "reply"}}}})
	if dm.scroll != 0 {
		t.Fatalf("refresh changed manual scroll to %d", dm.scroll)
	}
}
