package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const replyTestUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
const targetTestUID = "01BBBBBBBBBBBBBBBBBBBBBBBB"
const commentEditTime = "2026-10-07T12:00:00.123456789Z"

func TestFoldCommentReplySnapshotsAndEdits(t *testing.T) {
	for _, typ := range []string{"issue.commented", "issue.created", "issue.snapshot"} {
		t.Run(typ, func(t *testing.T) {
			comment := fmt.Sprintf(`{"comment_uid":%q,"author":"worker","teammate":"peer","body":"Original","created_at":"2026-10-07T11:00:00Z","reply_to_uid":%q,"reply_kind":"refute"}`, replyTestUID, targetTestUID)
			payload := comment
			if typ != "issue.commented" {
				payload = `{"uid":"issue-1","comments":[` + comment + `]}`
			}
			creation := testEvent(typ, 2, payload)
			edit := testEvent("issue.comment_edited", 3, fmt.Sprintf(`{"comment_uid":%q,"body":"Revised","edited_at":%q}`, replyTestUID, commentEditTime))
			got := FoldEvents([]FoldEvent{edit, creation}).Comments[replyTestUID]
			require.Equal(t, targetTestUID, got.ReplyToUID)
			require.Equal(t, "refute", got.ReplyKind)
			require.Equal(t, commentEditTime, got.EditedAt)
			require.Equal(t, "Revised", got.Body)
			edit.HLCPhysicalMS = 1
			got = FoldEvents([]FoldEvent{creation, edit}).Comments[replyTestUID]
			require.Equal(t, "worker", got.Author)
			require.Equal(t, targetTestUID, got.ReplyToUID)
			require.Equal(t, "Revised", got.Body)
			require.Equal(t, commentEditTime, got.EditedAt)
		})
	}
}

func TestFoldCommentSnapshotCarriesEditTime(t *testing.T) {
	payload := fmt.Sprintf(`{"uid":"issue-1","comments":[{"comment_uid":%q,"author":"worker","body":"Edited","created_at":"2026-10-07T11:00:00Z","edited_at":%q,"reply_to_uid":%q,"reply_kind":"reply"}]}`, replyTestUID, commentEditTime, targetTestUID)
	got := FoldEvents([]FoldEvent{testEvent("issue.snapshot", 1, payload)}).Comments[replyTestUID]
	require.Equal(t, commentEditTime, got.EditedAt)
	require.Equal(t, targetTestUID, got.ReplyToUID)
	require.Equal(t, "reply", got.ReplyKind)
}

func FuzzFederationCommentReplyValidation(f *testing.F) {
	f.Add(targetTestUID, "reply", commentEditTime)
	f.Add("short", "reply", commentEditTime)
	f.Add(targetTestUID, "answer", commentEditTime)
	f.Fuzz(func(t *testing.T, target, kind, edited string) {
		payload := fmt.Sprintf(`{"comment_uid":%q,"reply_to_uid":%q,"reply_kind":%q,"edited_at":%q}`, replyTestUID, target, kind, edited)
		standalone := ValidateFederationEntries("issue.commented", "event", jsontext.Value(payload))
		snapshot := ValidateFederationEntries("issue.snapshot", "event", jsontext.Value(`{"comments":[`+payload+`]}`))
		require.Equal(t, standalone == nil, snapshot == nil)
		if target == "short" || kind == "answer" || edited == "bad time" {
			require.ErrorIs(t, standalone, ErrFederationIngestValidation)
		}
	})
}

func TestFederationCommentReplyValidation(t *testing.T) {
	for _, payload := range []string{`{"reply_to_uid":42,"reply_kind":"reply"}`, `{"reply_to_uid":null,"reply_kind":"reply"}`, fmt.Sprintf(`{"comment_uid":%q,"reply_to_uid":%q,"reply_kind":"reply"}`, replyTestUID, replyTestUID), fmt.Sprintf(`{"comment_uid":%q,"reply_to_uid":%q,"reply_kind":"answer"}`, replyTestUID, targetTestUID), `{"edited_at":42}`, `{"edited_at":"bad"}`} {
		require.ErrorIs(t, ValidateFederationEntries("issue.commented", "event", jsontext.Value(payload)), ErrFederationIngestValidation, payload)
		require.ErrorIs(t, ValidateFederationEntries("issue.snapshot", "event", jsontext.Value(`{"comments":[`+payload+`]}`)), ErrFederationIngestValidation, payload)
	}
	for _, payload := range []string{`{"edited_at":42}`, `{"edited_at":"bad"}`} {
		require.ErrorIs(t, ValidateFederationEntries("issue.comment_edited", "event", jsontext.Value(payload)), ErrFederationIngestValidation)
	}
	for _, payload := range []string{`{"comment_uid":"comment-1"}`, `{"comment_uid":"comment-1","reply_to_uid":null,"reply_kind":null,"edited_at":null}`} {
		require.NoError(t, ValidateFederationEntries("issue.commented", "event", jsontext.Value(payload)))
	}
}

func TestFoldReplyNotificationIsExplicitMetadataOnly(t *testing.T) {
	creation := testEvent("issue.created", 1, `{"uid":"issue-1","title":"Task","author":"worker"}`)
	reply := testEvent("issue.commented", 2, fmt.Sprintf(`{"comment_uid":%q,"body":"Answer","reply_to_uid":%q,"reply_kind":"reply"}`, replyTestUID, targetTestUID))
	state := FoldEvents([]FoldEvent{creation, reply})
	require.NotContains(t, string(state.IssueMetadata["issue-1"]), "notify.")
	notification := testEvent("issue.metadata_updated", 3, `{"diff":{"notify.worker":{"from":null,"to":{"state":"requested","re":"`+replyTestUID+`"}}}}`)
	linked := FoldEvents([]FoldEvent{creation, reply, notification})
	// An older producer omits the link, but the metadata event has the same effect.
	legacy := reply
	legacy.Payload = jsontext.Value(fmt.Sprintf(`{"comment_uid":%q,"body":"Answer"}`, replyTestUID))
	old := FoldEvents([]FoldEvent{creation, legacy, notification})
	require.Equal(t, old.IssueMetadata["issue-1"], linked.IssueMetadata["issue-1"])
	require.NotEmpty(t, linked.IssueMetadata["issue-1"])
}

// Creation contribution is display data; edits must never replace it.
func TestFoldCommentDisplayContribution(t *testing.T) {
	for _, kind := range []string{"issue.commented", "issue.created", "issue.snapshot"} {
		for _, tc := range []struct{ raw, want string }{
			{`"attributed_teammate":"writer","teammate":"route"`, "writer"},
			{`"teammate":"legacy"`, "legacy"},
			{`"attributed_teammate":"bad/handle","teammate":"route"`, ""},
			{`"attributed_teammate":42`, ""},
			{`"attributed_teammate":""`, ""},
		} {
			t.Run(kind+tc.raw, func(t *testing.T) {
				body := `{"comment_uid":"comment-1","author":"actor","body":"original",` + tc.raw + `}`
				if kind != "issue.commented" {
					body = `{"uid":"issue-1","comments":[` + body + `]}`
				}
				creation := testEvent(kind, 1, body)
				edit := testEvent("issue.comment_edited", 2, `{"comment_uid":"comment-1","body":"edited","attributed_teammate":"editor"}`)
				got := FoldEvents([]FoldEvent{creation, edit}).Comments["comment-1"]
				data, err := json.Marshal(got)
				require.NoError(t, err)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(data, &fields))
				require.Equal(t, tc.want, fields["AttributedTeammate"])
			})
		}
	}
}

func TestFoldCommentExplicitDisplayOverridesLegacyFallback(t *testing.T) {
	const commentUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	legacy := testEvent("issue.commented", 1, fmt.Sprintf(
		`{"comment_uid":%q,"author":"worker","teammate":"route","body":"note","created_at":"2026-10-07T11:00:00Z"}`,
		commentUID,
	))
	snapshot := testEvent("issue.snapshot", 2, fmt.Sprintf(
		`{"uid":"issue-1","comments":[{"comment_uid":%q,"author":"worker","teammate":"route","attributed_teammate":"","body":"note","created_at":"2026-10-07T11:00:00Z"}]}`,
		commentUID,
	))

	got := FoldEvents([]FoldEvent{legacy, snapshot}).Comments[commentUID]
	require.Empty(t, got.AttributedTeammate, "explicit empty contribution must replace a legacy routing fallback")
}

func TestCanonicalCommentUIDDoesNotUnicodeCaseFoldMalformedID(t *testing.T) {
	valid := "01" + strings.Repeat("S", 24)
	malformed := "01ſ" + strings.Repeat("S", 23)
	require.Equal(t, valid, CanonicalCommentUID(strings.ToLower(valid)))
	require.Equal(t, malformed, CanonicalCommentUID(malformed), "non-ASCII opaque IDs must not alias ULIDs through Unicode case conversion")
}

func TestValidateCommentReplyRejectsCaseVariantSelfTarget(t *testing.T) {
	const commentUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	require.Error(t, ValidateCommentReply(commentUID, "01aaaaaaaaaaaaaaaaaaaaaaaa", "reply"))
}

func TestFederationCommentUIDRequired(t *testing.T) {
	for _, test := range []struct {
		kind    string
		payload string
	}{
		{kind: "issue.commented", payload: `{"body":"orphan comment"}`},
		{kind: "issue.commented", payload: `{"comment_uid":"","body":"orphan comment"}`},
		{kind: "issue.comment_edited", payload: `{"body":"orphan edit"}`},
		{kind: "issue.comment_edited", payload: `{"comment_uid":"","body":"orphan edit"}`},
		{kind: "issue.created", payload: `{"uid":"issue-1","comments":[{"body":"orphan comment"}]}`},
		{kind: "issue.snapshot", payload: `{"uid":"issue-1","comments":[{"comment_uid":"","body":"orphan comment"}]}`},
	} {
		t.Run(test.kind+test.payload, func(t *testing.T) {
			require.ErrorIs(t, ValidateFederationEntries(test.kind, "event", jsontext.Value(test.payload)), ErrFederationIngestValidation)
		})
	}
}

func TestFederationCreatedAndSnapshotRejectCanonicalCommentUIDDuplicates(t *testing.T) {
	const upperUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	const lowerUID = "01aaaaaaaaaaaaaaaaaaaaaaaa"
	payload := `{"uid":"issue-1","comments":[` +
		`{"comment_uid":"` + upperUID + `","author":"worker","body":"first","created_at":"2026-10-07T11:00:00Z","attributed_teammate":"writer"},` +
		`{"comment_uid":"` + lowerUID + `","author":"worker","body":"second","created_at":"2026-10-07T11:00:00Z","attributed_teammate":""}]}`
	for _, eventType := range []string{"issue.created", "issue.snapshot"} {
		t.Run(eventType, func(t *testing.T) {
			require.ErrorIs(t,
				ValidateFederationEntries(eventType, "snapshot-1", jsontext.Value(payload)),
				ErrFederationIngestValidation,
			)
		})
	}
}
