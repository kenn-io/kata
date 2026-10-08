package db_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Confirm/refute evidence requires forty normalized Unicode characters.
func FuzzLocalReplyEvidenceFloor(f *testing.F) {
	f.Add("confirm", strings.Repeat("é", 39))
	f.Add("refute", strings.Repeat("é", 40))
	f.Add("reply", "Short answer")
	f.Fuzz(func(t *testing.T, kind, body string) {
		if kind != "confirm" && kind != "refute" && kind != "reply" && kind != "supersede" {
			t.Skip()
		}
		p := db.CreateCommentParams{ReplyToUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", ReplyKind: kind, Body: body, Author: "worker", ValidateReply: true}
		err := db.ValidateLocalCommentReply("01BBBBBBBBBBBBBBBBBBBBBBBB", p, db.Issue{ProjectID: 1, Status: "open"}, &db.Issue{ProjectID: 1, Status: "closed"}, nil)
		if (kind == "confirm" || kind == "refute") && len([]rune(strings.Join(strings.Fields(body), " "))) < 40 {
			require.Error(t, err)
		}
		if (kind == "reply" || kind == "supersede") && strings.TrimSpace(body) != "" {
			require.NoError(t, err)
		}
	})
}

func TestLocalReplySelfTargetRefused(t *testing.T) {
	const id = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	p := db.CreateCommentParams{ReplyToUID: id, ReplyKind: "reply", Body: "Evidence", ValidateReply: true}
	err := db.ValidateLocalCommentReply(id, p, db.Issue{ProjectID: 1, Status: "open"}, &db.Issue{ProjectID: 1, Status: "open"}, nil)
	require.Error(t, err)
}
