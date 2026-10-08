package jsonl_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestCommentReplyJSONLRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var source, target db.Storage
			if backend == "sqlite" {
				var err error
				source, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
				require.NoError(t, err)
				target, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
				require.NoError(t, err)
			} else {
				if testing.Short() {
					t.Skip("requires PostgreSQL")
				}
				dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
				t.Cleanup(cleanup)
				var err error
				source, err = pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: "reply_source", SchemaMode: pgstore.SchemaModeBootstrap})
				require.NoError(t, err)
				target, err = pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: "reply_target", SchemaMode: pgstore.SchemaModeBootstrap})
				require.NoError(t, err)
			}
			t.Cleanup(func() { _ = source.Close(); _ = target.Close() })
			roundTripReply(t, source, target)
		})
	}
}

func roundTripReply(t *testing.T, source, target db.Storage) {
	t.Helper()
	ctx := context.Background()
	p, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	c, _, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Reply", ReplyToUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", ReplyKind: "reply"})
	require.NoError(t, err)
	c, _, _, err = source.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: c.UID, Actor: "worker", Body: "Edited reply"})
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &out, jsonl.ExportOptions{}))
	require.Contains(t, out.String(), `"reply_to_uid"`)
	require.Contains(t, out.String(), `"edited_at"`)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(out.Bytes()), target))
	comments, err := target.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, c.ReplyToUID, comments[0].ReplyToUID)
	require.Equal(t, c.ReplyKind, comments[0].ReplyKind)
	require.True(t, c.EditedAt.Equal(*comments[0].EditedAt))
	require.Equal(t, c.Body, comments[0].Body)
}

func TestCommentReplyImportPreservesEditPrecision(t *testing.T) {
	s := openImportTargetDB(t)
	const edited = "2026-10-07T12:00:00.123456789Z"
	input := `{"kind":"meta","data":{"key":"export_version","value":"31"}}
{"kind":"project","data":{"id":1,"uid":"01AAAAAAAAAAAAAAAAAAAAAAAA","name":"example-project","created_at":"2026-10-07T11:00:00Z"}}
{"kind":"issue","data":{"id":1,"uid":"01BBBBBBBBBBBBBBBBBBBBBBBB","project_id":1,"short_id":"bbbb","title":"Task","author":"worker","status":"open","created_at":"2026-10-07T11:00:00Z","updated_at":"2026-10-07T11:00:00Z"}}
{"kind":"comment","data":{"id":1,"uid":"01CCCCCCCCCCCCCCCCCCCCCCCC","issue_id":1,"author":"worker","body":"Reply","created_at":"2026-10-07T11:00:00Z","reply_to_uid":"01DDDDDDDDDDDDDDDDDDDDDDDD","reply_kind":"confirm","edited_at":"` + edited + `"}}
`
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewBufferString(input), s))
	comments, err := s.CommentsByIssue(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.NotNil(t, comments[0].EditedAt)
	require.Equal(t, edited, comments[0].EditedAt.Format(time.RFC3339Nano))
	require.Equal(t, "01DDDDDDDDDDDDDDDDDDDDDDDD", comments[0].ReplyToUID)
}

func TestCommentReplySchema30Cutover(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	p, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	i, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	c, _, err := s.CreateComment(ctx, db.CreateCommentParams{IssueID: i.ID, Author: "worker", Body: "Legacy finding"})
	require.NoError(t, err)
	// Build the real released comment table shape; ephemeral fixture DDL only.
	_, err = s.ExecContext(ctx, `DROP INDEX idx_comments_reply_to;
 ALTER TABLE comments RENAME TO comments_current;
 CREATE TABLE comments(id INTEGER PRIMARY KEY AUTOINCREMENT,uid TEXT NOT NULL UNIQUE,issue_id INTEGER NOT NULL REFERENCES issues(id),author TEXT NOT NULL,body TEXT NOT NULL,created_at DATETIME NOT NULL,teammate TEXT,CHECK(length(uid)=26),CHECK(length(trim(author))>0),CHECK(length(trim(body))>0));
 INSERT INTO comments(id,uid,issue_id,author,body,created_at,teammate) SELECT id,uid,issue_id,author,body,created_at,teammate FROM comments_current;
 DROP TABLE comments_current;
 CREATE INDEX idx_comments_issue ON comments(issue_id,created_at);
 UPDATE meta SET value='30' WHERE key='schema_version';`)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	restored, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restored.Close() })
	comments, err := restored.CommentsByIssue(ctx, i.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, c.UID, comments[0].UID)
	require.Empty(t, comments[0].ReplyToUID)
	require.Empty(t, comments[0].ReplyKind)
	require.Nil(t, comments[0].EditedAt)
}
