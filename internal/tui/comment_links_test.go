package tui

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
)

func FuzzCommentEntryRoundTrip(f *testing.F) {
	f.Add("01AAAAAAAAAAAAAAAAAAAAAAAA", "c:aaaaaa", "source-issue", "reply")
	f.Fuzz(func(t *testing.T, uid, handle, issue, kind string) {
		if !utf8.ValidString(uid) || !utf8.ValidString(handle) || !utf8.ValidString(issue) || !utf8.ValidString(kind) {
			return
		}
		c := CommentEntry{UID: uid, Handle: handle, Reply: &commentref.Link{UID: uid, IssueUID: issue, Kind: kind}}
		bs, err := json.Marshal(c)
		require.NoError(t, err)
		var got CommentEntry
		require.NoError(t, json.Unmarshal(bs, &got))
		require.Equal(t, c.UID, got.UID)
		require.Equal(t, c.Handle, got.Handle)
		require.Equal(t, c.Reply, got.Reply)
	})
}

func TestCommentLinksRenderAndNavigate(t *testing.T) {
	edited := time.Now()
	c := CommentEntry{UID: "source-comment", Handle: "c:abc123", Author: "worker", Body: "Answer", EditedAt: &edited, Reply: &commentref.Link{UID: "target-comment", Handle: "abcd:def456", IssueUID: "target-issue", ProjectID: 9, Kind: "reply", TargetEdited: true}, Backlinks: []commentref.Link{{UID: "backlink-comment", IssueUID: "backlink-issue", Handle: "abcd:ghi789", ProjectID: 9, Kind: "confirm"}}}
	rendered := stripANSI(renderCommentsTab([]CommentEntry{c}, 100, 30, 0, tabState{}))
	for _, want := range []string{"c:abc123", "↳ Replies to abcd:def456", "Confirmations 1", "(edited)", "Target edited after this reply"} {
		require.True(t, strings.Contains(rendered, want), rendered)
	}
	dm := detailModel{activeTab: tabComments, comments: []CommentEntry{c}}
	target, ok := dm.jumpTarget()
	require.True(t, ok)
	require.Equal(t, "target-issue", target.ref)
	require.Equal(t, "target-comment", target.commentUID)
	require.Equal(t, int64(9), target.projectID)
}

func TestTypedReplyFormCapturesTarget(t *testing.T) {
	m := Model{view: viewDetail, detail: detailModel{scopePID: 7, gen: 3, activeTab: tabComments, issue: &Issue{UID: "source-issue", ShortID: "aaaa", Status: "open"}, comments: []CommentEntry{{UID: "target-comment", Handle: "c:abc123"}}}}
	m = m.openTypedReplyForm("refute")
	require.Equal(t, "target-comment", m.input.target.replyUID)
	require.Equal(t, "source-issue", m.input.target.issueUID)
	require.Equal(t, "refute", m.input.target.replyKind)
	m.detail.comments[0].UID = "changed-selection"
	require.Equal(t, "target-comment", m.input.target.replyUID)
	m.input.fields[0].area.SetValue("Too short")
	m, cmd := m.commitFormInput(inputCommentForm)
	require.Nil(t, cmd)
	require.Contains(t, m.input.err, "40")
}

func forcedReplyDraft(t *testing.T, body string) Model {
	t.Helper()
	m := Model{view: viewDetail, detail: detailModel{scopePID: 7, activeTab: tabComments, issue: &Issue{UID: "source-issue", Status: "open"}, comments: []CommentEntry{{UID: "target-comment", Handle: "c:abc123"}}}}
	m = m.openTypedReplyForm("reply")
	m.input.fields[0].area.SetValue(body)
	m, cmd := m.commitFormInput(inputCommentForm)
	require.NotNil(t, cmd)
	m.input.saving = false
	m.input.err = "duplicate_reply: existing c:def456"
	m, cmd = m.routeInputKey(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	require.True(t, m.input.target.forceReply)
	// A transport failure keeps the draft open for retry.
	m.input.saving = false
	m.input.err = "temporary transport failure"
	return m
}

func assertForcedReplyDraft(t *testing.T, original, next string, changeKind bool) {
	t.Helper()
	m := forcedReplyDraft(t, original)
	original = m.input.target.submittedBody
	key := m.input.target.idempotencyKey
	m.input.fields[0].area.SetValue(next)
	next = m.input.fieldValue(fieldComment)
	if changeKind {
		m.input.target.replyKind = "supersede"
	}
	m, cmd := m.commitFormInput(inputCommentForm)
	require.NotNil(t, cmd)
	changed := original != next || changeKind
	require.Equal(t, !changed, m.input.target.forceReply, "force must be bound to the explicitly forced draft")
	if changed {
		require.NotEqual(t, key, m.input.target.idempotencyKey)
	} else {
		require.Equal(t, key, m.input.target.idempotencyKey)
	}
}

func TestTypedReplyForceClearsAfterBodyEdit(t *testing.T) {
	assertForcedReplyDraft(t, "a", "b", false)
}

func TestTypedReplyForceClearsAfterKindEdit(t *testing.T) {
	assertForcedReplyDraft(t, "a", "a", true)
}

func TestTypedReplyForceSurvivesUnchangedRetry(t *testing.T) {
	assertForcedReplyDraft(t, "a", "a", false)
}

func TestTypedReplyKindShortcutClearsForce(t *testing.T) {
	m := forcedReplyDraft(t, strings.Repeat("Evidence ", 6))
	m, cmd := m.routeInputKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	require.Nil(t, cmd)
	require.Equal(t, "confirm", m.input.target.replyKind)
	require.False(t, m.input.target.forceReply)
}

func TestTypedReplyBodyShortcutClearsForce(t *testing.T) {
	m := forcedReplyDraft(t, "a")
	m, _ = m.routeInputKey(runeKey('b'))
	require.False(t, m.input.target.forceReply)
}

func FuzzTypedReplyForceDraft(f *testing.F) {
	f.Add("a", "b", false)
	f.Add("a", "a", true)
	f.Add("a", "a", false)
	f.Fuzz(func(t *testing.T, original, next string, changeKind bool) {
		if !utf8.ValidString(original) || !utf8.ValidString(next) || strings.TrimSpace(original) == "" || strings.TrimSpace(next) == "" {
			return
		}
		assertForcedReplyDraft(t, original, next, changeKind)
	})
}

func TestTypedReplyRequiresWritableCapabilities(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale", true: "read-only"}[ready], func(t *testing.T) {
			m := initialModel(Options{})
			m.view = viewDetail
			m.detail = detailModel{activeTab: tabComments, issue: &Issue{UID: "source-issue", Status: "open"}, comments: []CommentEntry{{UID: "target-comment", Handle: "c:abc123"}}}
			m.authCapabilitiesRequired = true
			m.authCapabilitiesReady = ready
			m.issueScoped = true
			m.scopedWritable = false
			model, _ := m.Update(runeKey('R'))
			require.Equal(t, inputNone, model.(Model).input.kind)
		})
	}
}

func TestIncomingRelationSummaryAndEvidencePicker(t *testing.T) {
	c := CommentEntry{UID: "original", Handle: "c:aaaaaa", Author: "worker", Body: "Original finding", Backlinks: []commentref.Link{
		{UID: "confirmation", IssueUID: "other", Handle: "bbbb:111111", Kind: "confirm", Author: "worker-a", Teammate: "reviewer"},
		{UID: "refutation", IssueUID: "other", Handle: "bbbb:222222", Kind: "refute", Author: "worker-b"},
		{UID: "replacement-one", IssueUID: "other", Handle: "bbbb:333333", Kind: "supersede"},
		{UID: "replacement-two", IssueUID: "other", Handle: "bbbb:444444", Kind: "supersede"},
	}}
	rendered := stripANSI(renderCommentsTab([]CommentEntry{c}, 80, 40, 0, tabState{}))
	for _, label := range []string{"Confirmations 1", "Refutations 1", "Superseding replies 2", "worker-a / reviewer", "Original finding"} {
		require.Contains(t, rendered, label)
	}
	require.NotContains(t, rendered, "Replies 0")
	dm := detailModel{activeTab: tabComments, detailFocus: focusActivity, comments: []CommentEntry{c}}
	dm, _, _ = dm.handleNavKey(runeKey(']'), newKeymap(), nil)
	target, ok := dm.jumpTarget()
	require.True(t, ok)
	require.Equal(t, "refutation", target.commentUID)
}

func FuzzIncomingRelationCounts(f *testing.F) {
	f.Add(uint8(1), uint8(2), uint8(1), uint8(2), false)
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), true)
	f.Fuzz(func(t *testing.T, reply, confirm, refute, supersede uint8, partial bool) {
		counts := []uint8{reply % 4, confirm % 4, refute % 4, supersede % 4}
		kinds := []string{"reply", "confirm", "refute", "supersede"}
		labels := []string{"Replies", "Confirmations", "Refutations", "Superseding replies"}
		links := []map[string]any{}
		for i, n := range counts {
			for range int(n) {
				links = append(links, map[string]any{"kind": kinds[i], "uid": fmt.Sprintf("reply-%d", len(links)), "issue_uid": "other", "handle": "bbbb:111111", "body": "Evidence body", "author": "worker"})
			}
		}
		raw, err := json.Marshal(map[string]any{"uid": "original", "body": "Original finding", "backlinks": links, "backlinks_truncated": partial})
		require.NoError(t, err)
		var c CommentEntry
		require.NoError(t, json.Unmarshal(raw, &c))
		rendered := stripANSI(renderCommentsTab([]CommentEntry{c}, 120, 80, 0, tabState{}))
		for i, n := range counts {
			if n == 0 {
				require.NotContains(t, rendered, labels[i]+" 0")
				continue
			}
			suffix := ""
			if partial {
				suffix = "+"
			}
			require.Contains(t, rendered, fmt.Sprintf("%s %d%s", labels[i], n, suffix))
		}
		require.Contains(t, rendered, "Original finding")
	})
}

func TestIncomingEvidenceChronologyAndConflicts(t *testing.T) {
	early := time.Date(2026, 10, 8, 10, 30, 0, 0, time.UTC)
	c := CommentEntry{UID: "original", Handle: "c:aaaaaa", Body: "Original finding", Backlinks: []commentref.Link{
		{UID: "late", IssueUID: "other", Handle: "bbbb:222222", Kind: "confirm", Body: "Later reproduction", Author: "worker-b", CreatedAt: early.Add(time.Minute), TargetEdited: true},
		{UID: "early", IssueUID: "other", Handle: "bbbb:111111", Kind: "confirm", Body: "Earlier reproduction", Author: "worker-a", Teammate: "reviewer", CreatedAt: early},
		{UID: "refute", IssueUID: "other", Handle: "bbbb:333333", Kind: "refute", Body: "Counterexample", Author: "worker-c", CreatedAt: early.Add(2 * time.Minute)},
	}}
	rendered := stripANSI(renderCommentsTab([]CommentEntry{c}, 100, 50, 0, tabState{}))
	require.Less(t, strings.Index(rendered, "Earlier reproduction"), strings.Index(rendered, "Later reproduction"))
	require.Contains(t, rendered, "worker-a / reviewer")
	require.Contains(t, rendered, "Target edited after this reply")
	require.Contains(t, rendered, "Confirmations 2 | Refutations 1")
	other := stripANSI(renderCommentsTab([]CommentEntry{c}, 100, 50, 0, tabState{commentLinkCursor: 2}))
	require.Contains(t, other, "Counterexample")
	require.Contains(t, other, "Original finding")
	require.Contains(t, other, "Confirmations 2 | Refutations 1")
	narrow := stripANSI(renderCommentsTab([]CommentEntry{c}, 28, 80, 0, tabState{}))
	for line := range strings.SplitSeq(narrow, "\n") {
		require.LessOrEqual(t, runewidth.StringWidth(line), 28)
	}
	require.Contains(t, narrow, "Confirmations 2")
	require.Contains(t, narrow, "Refutations 1")
}

func TestCommentEvidenceJumpAndReturnRestoresSelection(t *testing.T) {
	m := newTestModel()
	m.view = viewDetail
	m.detail = detailModel{issue: &Issue{UID: "source-issue", ShortID: "aaaa", Status: "open"}, scopePID: 7, activeTab: tabComments, detailFocus: focusActivity, gen: 1, tabCursor: 1, commentLinkCursor: 1, comments: []CommentEntry{
		{UID: "first"},
		{UID: "original", Backlinks: []commentref.Link{{UID: "confirmation", IssueUID: "other-issue", ProjectID: 7, Kind: "confirm"}, {UID: "refutation", IssueUID: "other-issue", ProjectID: 7, Kind: "refute"}}},
	}}
	m.nextGen = 1
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	jump := cmd().(jumpDetailMsg)
	require.Equal(t, "refutation", jump.commentUID)
	m, _ = updateModel(m, jump)
	require.Len(t, m.detail.navStack, 1)
	m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, issue: &Issue{UID: "other-issue", ShortID: "bbbb"}})
	m, _ = updateModel(m, commentsFetchedMsg{gen: m.detail.gen, comments: []CommentEntry{{UID: "other"}, {UID: "refutation"}}})
	require.Equal(t, 1, m.detail.tabCursor)
	require.Equal(t, focusActivity, m.detail.detailFocus)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, "source-issue", m.detail.issue.UID)
	require.Equal(t, 1, m.detail.tabCursor)
	require.Equal(t, 1, m.detail.commentLinkCursor)
	require.Equal(t, focusActivity, m.detail.detailFocus)
}
