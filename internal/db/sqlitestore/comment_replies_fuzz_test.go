package sqlitestore_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// CreateComment accepts only paired reply fields, the reply/confirm/refute/
// supersede kinds, and a 26-character target. The target need not exist yet.
func FuzzCreateCommentReplyValidation(f *testing.F) {
	f.Add("01AAAAAAAAAAAAAAAAAAAAAAAA", "reply")
	f.Add("01AAAAAAAAAAAAAAAAAAAAAAAA", "answer")
	f.Add("", "")
	f.Add("01BBBBBBBBBBBBBBBBBBBBBBBB", "")
	f.Add("", "confirm")
	f.Add("short", "refute")
	f.Fuzz(func(t *testing.T, target, kind string) {
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
		_, _, err = s.CreateComment(ctx, db.CreateCommentParams{
			IssueID: i.ID, Author: "worker", Body: "Finding", ReplyToUID: target, ReplyKind: kind,
		})
		validKind := kind == "reply" || kind == "confirm" || kind == "refute" || kind == "supersede"
		valid := (target == "") == (kind == "") && (target == "" || (utf8.RuneCountInString(target) == 26 && validKind))
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

// Snapshot comments use one fixed-width timestamp layout, so equal edits hash
// the same regardless of trailing zeros.
func TestFederationSnapshotEditedAtIsFixedWidth(t *testing.T) {
	ctx := t.Context()
	s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	p, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	c, _, err := s.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Reply"})
	require.NoError(t, err)
	_, err = s.ExecContext(ctx, `UPDATE comments SET edited_at = '2026-10-07T12:00:00.100Z' WHERE uid = ?`, c.UID)
	require.NoError(t, err)
	_, err = s.EnableProjectFederation(ctx, p.ID, "worker")
	require.NoError(t, err)
	events, err := s.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 100})
	require.NoError(t, err)
	for _, event := range events {
		if event.Type != "issue.snapshot" {
			continue
		}
		var snapshot struct {
			Comments []struct {
				EditedAt string `json:"edited_at"`
			} `json:"comments"`
		}
		require.NoError(t, json.Unmarshal([]byte(event.Payload), &snapshot))
		require.Len(t, snapshot.Comments, 1)
		require.Equal(t, "2026-10-07T12:00:00.100000000Z", snapshot.Comments[0].EditedAt)
		return
	}
	require.Fail(t, "enabling federation should emit an issue snapshot")
}
