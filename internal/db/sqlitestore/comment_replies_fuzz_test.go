package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Schema 31 contract: paired nullable reply fields, the reply/confirm/refute/
// supersede kinds, a 26-character target other than this comment, and no target
// foreign key.
func FuzzCommentReplyConstraints(f *testing.F) {
	f.Add("01AAAAAAAAAAAAAAAAAAAAAAAA", "reply", true, true)
	f.Add("01AAAAAAAAAAAAAAAAAAAAAAAA", "answer", true, true)
	f.Add("", "", false, false)
	f.Add("01BBBBBBBBBBBBBBBBBBBBBBBB", "confirm", true, true)
	f.Add("short", "refute", true, true)
	f.Add("01AAAAAAAAAAAAAAAAAAAAAAAA", "invalid", true, true)
	f.Fuzz(func(t *testing.T, target, kind string, hasTarget, hasKind bool) {
		if !utf8.ValidString(target) || !utf8.ValidString(kind) {
			t.Skip()
		}
		ctx := context.Background()
		s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		p, err := s.CreateProject(ctx, "example-project")
		require.NoError(t, err)
		i, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Task", Author: "worker"})
		require.NoError(t, err)
		const own = "01BBBBBBBBBBBBBBBBBBBBBBBB"
		var targetValue, kindValue any
		if hasTarget {
			targetValue = target
		}
		if hasKind {
			kindValue = kind
		}
		_, err = s.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body,reply_to_uid,reply_kind) VALUES(?,?,'worker','Finding',?,?)`, own, i.ID, targetValue, kindValue)
		valid := hasTarget == hasKind && (!hasTarget || (utf8.RuneCountInString(target) == 26 && target != own && (kind == "reply" || kind == "confirm" || kind == "refute" || kind == "supersede")))
		if valid {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	})
}

// Purging an unrelated target must not remove the reply's replay identity.
func FuzzCommentReplyPurgeRetainsEvent(f *testing.F) {
	f.Add(uint8(0))
	f.Add(uint8(3))
	f.Fuzz(func(t *testing.T, choice uint8) {
		ctx := t.Context()
		s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		p, err := s.CreateProject(ctx, "example-project")
		require.NoError(t, err)
		source, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Reply", Author: "worker"})
		require.NoError(t, err)
		target, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Finding", Author: "worker"})
		require.NoError(t, err)
		finding, _, err := s.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "worker", Body: "Finding"})
		require.NoError(t, err)
		kinds := [...]string{"reply", "confirm", "refute", "supersede"}
		_, event, err := s.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Reply", ReplyToUID: finding.UID, ReplyKind: kinds[choice%4]})
		require.NoError(t, err)
		_, err = s.PurgeIssue(ctx, target.ID, "worker", nil)
		require.NoError(t, err)
		retained, err := s.EventsByUIDs(ctx, p.ID, []string{event.UID})
		require.NoError(t, err)
		require.Len(t, retained, 1)
		require.Nil(t, retained[0].RelatedIssueID)
		require.Equal(t, event.RelatedIssueUID, retained[0].RelatedIssueUID)
		require.Equal(t, event.ContentHash, retained[0].ContentHash)
	})
}
