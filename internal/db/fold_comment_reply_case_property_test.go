package db

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const commentUIDAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func drawCommentULID(ht *hegel.T) string {
	first := hegel.Draw(ht, hegel.SampledFrom([]string{"0", "1", "2", "3", "4", "5", "6", "7"}))
	tail := []byte(hegel.Draw(ht, hegel.Text().Alphabet(commentUIDAlphabet).MinSize(25).MaxSize(25)))
	return first + string(tail)
}

func drawCaseVariantCommentUID(ht *hegel.T, uid string) string {
	variant := []byte(uid)
	lowered := false
	for index := range variant {
		if variant[index] < 'A' || variant[index] > 'Z' || !hegel.Draw(ht, hegel.Booleans()) {
			continue
		}
		variant[index] += 'a' - 'A'
		lowered = true
	}
	if !lowered {
		for index := range variant {
			if variant[index] >= 'A' && variant[index] <= 'Z' {
				variant[index] += 'a' - 'A'
				break
			}
		}
	}
	return string(variant)
}

func TestFoldCommentReplyUsesCanonicalUIDProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		issueUID := drawCommentULID(ht)
		targetUID := drawCommentULID(ht)
		replySuffix := byte('0')
		if targetUID[len(targetUID)-1] == replySuffix {
			replySuffix = '1'
		}
		replyUID := targetUID[:len(targetUID)-1] + string(replySuffix)
		replyToUID := drawCaseVariantCommentUID(ht, targetUID)

		events := []FoldEvent{
			{
				UID:           "target-comment",
				Type:          "issue.commented",
				IssueUID:      issueUID,
				HLCPhysicalMS: 1,
				Payload:       jsontext.Value(fmt.Sprintf(`{"comment_uid":%q,"author":"worker","body":"target","created_at":"2026-10-07T11:00:00Z"}`, targetUID)),
			},
			{
				UID:           "reply-comment",
				Type:          "issue.commented",
				IssueUID:      issueUID,
				HLCPhysicalMS: 2,
				Payload:       jsontext.Value(fmt.Sprintf(`{"comment_uid":%q,"author":"worker","body":"reply","created_at":"2026-10-07T11:01:00Z","reply_to_uid":%q,"reply_kind":"reply"}`, replyUID, replyToUID)),
			},
		}

		projection := FoldEvents(events)
		canonicalTargetUID := CanonicalCommentUID(targetUID)
		target, ok := projection.Comments[canonicalTargetUID]
		require.True(ht, ok)
		require.Equal(ht, canonicalTargetUID, target.UID)

		reply, ok := projection.Comments[CanonicalCommentUID(replyUID)]
		require.True(ht, ok)
		require.Equal(ht, canonicalTargetUID, reply.ReplyToUID)
		require.Contains(ht, projection.Comments, reply.ReplyToUID)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
