package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestScopedCommentReplyRequiresBothIssues(t *testing.T) {
	source, target := int64(1), int64(2)
	targetUID := "01AAAAAAAAAAAAAAAAAAAAAAAA"
	event := db.Event{Type: "issue.commented", ProjectUID: "project", IssueID: &source, RelatedIssueID: &target, RelatedIssueUID: &targetUID, Payload: `{"comment_uid":"01BBBBBBBBBBBBBBBBBBBBBBBB","reply_to_uid":"01CCCCCCCCCCCCCCCCCCCCCCCC","reply_kind":"reply","idempotency_key":"secret"}`}
	require.False(t, issueScopedEventInScope(event, map[int64]struct{}{source: {}}, "project"))
	require.True(t, issueScopedEventInScope(event, map[int64]struct{}{source: {}, target: {}}, "project"))
	payload, ok := scopedEventPayload(event)
	require.True(t, ok)
	require.NotContains(t, payload, `"reply_to_uid"`,
		"the context-free projection cannot authorize the current reply target")
	require.NotContains(t, payload, `"reply_kind"`)
	require.NotContains(t, payload, "secret")
	event.RelatedIssueID = nil
	require.False(t, issueScopedEventInScope(event, map[int64]struct{}{source: {}, target: {}}, "project"), "an unresolved related UID must fail closed")
}

func FuzzScopedCommentWithVisibleSourceRequiresReset(f *testing.F) {
	f.Add(uint8(1), uint8(2))
	f.Add(uint8(17), uint8(23))
	f.Fuzz(func(t *testing.T, sourceSeed, targetSeed uint8) {
		sourceID := int64(sourceSeed) + 1
		targetID := int64(targetSeed) + 1
		if targetID == sourceID {
			targetID += 256
		}
		event := db.Event{
			Type: "issue.commented", ProjectUID: "project",
			IssueID: &sourceID, RelatedIssueID: &targetID,
		}
		reset, err := hiddenEventRequiresScopedReset(
			context.Background(), nil, event, map[int64]struct{}{sourceID: {}}, nil, "project",
		)
		require.NoError(t, err)
		require.True(t, reset, "a visible source comment with a hidden reply target must reset scoped readers")
	})
}

func FuzzIssueScopedEventRedactsReplyWithoutCurrentTargetLookup(f *testing.F) {
	f.Add("01CCCCCCCCCCCCCCCCCCCCCCCC", "reply")
	f.Add("01DDDDDDDDDDDDDDDDDDDDDDDD", "confirm")
	f.Fuzz(func(t *testing.T, replyToUID, replyKind string) {
		if len(replyToUID) > 256 || len(replyKind) > 64 {
			t.Skip()
		}
		payload, err := json.Marshal(struct {
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}{ReplyToUID: replyToUID, ReplyKind: replyKind})
		require.NoError(t, err)
		issueID := int64(1)
		event := db.Event{
			Type: "issue.commented", ProjectUID: "project", IssueID: &issueID,
			Payload: string(payload),
		}
		projected, ok := projectIssueScopedEvent(event, map[int64]struct{}{issueID: {}}, "project")
		require.True(t, ok)
		require.NotContains(t, projected.Payload, `"reply_to_uid"`,
			"a context-free projection cannot authorize a reply target")
		require.NotContains(t, projected.Payload, `"reply_kind"`,
			"reply kind must not outlive its unauthorized target")
	})
}

func TestProjectScopedCommentRepliesReturnsNonNilEmptySlice(t *testing.T) {
	comments, allowed, scoped, err := projectScopedCommentReplies(context.Background(), nil, nil)
	require.NoError(t, err)
	require.NotNil(t, comments)
	require.Empty(t, comments)
	require.Nil(t, allowed)
	require.False(t, scoped)
}

func TestRestoreScopedEventReplyFieldsInitializesNullProjection(t *testing.T) {
	targetUID := "01FFFFFFFFFFFFFFFFFFFFFFFF"
	original := `{"reply_to_uid":"` + targetUID + `","reply_kind":"reply"}`
	payload, err := restoreScopedEventReplyFields("null", original)
	require.NoError(t, err)
	require.JSONEq(t, original, payload)
}
