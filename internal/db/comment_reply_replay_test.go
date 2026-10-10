package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommentReplyReplayValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		comment *CommentExport
	}{
		{name: "missing kind", comment: &CommentExport{UID: replyTestUID, ReplyToUID: targetTestUID}},
		{name: "missing target", comment: &CommentExport{UID: replyTestUID, ReplyKind: "reply"}},
		{name: "self target", comment: &CommentExport{UID: replyTestUID, ReplyToUID: replyTestUID, ReplyKind: "reply"}},
		{name: "superseded answer kind", comment: &CommentExport{UID: replyTestUID, ReplyToUID: targetTestUID, ReplyKind: "answer"}},
		{name: "invalid edit time", comment: &CommentExport{UID: replyTestUID, EditedAt: "bad"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, ValidateImportRecords([]ImportRecord{test.comment}))
		})
	}
	require.NoError(t, ValidateImportRecords([]ImportRecord{&CommentExport{UID: replyTestUID, ReplyToUID: targetTestUID, ReplyKind: "reply", EditedAt: commentEditTime}}))
}
