package db

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelectedCommentGraphWorkIgnoresUnrelatedBodies(t *testing.T) {
	for _, unrelated := range []int{0, 256} {
		t.Run(fmt.Sprint(unrelated), func(t *testing.T) {
			ctx := t.Context()
			work := &referenceWork{}
			pool := sql.OpenDB(referenceConnector{work})
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			for _, query := range []string{
				`CREATE TABLE projects(id INTEGER PRIMARY KEY,uid TEXT,name TEXT,deleted_at TEXT)`,
				`CREATE TABLE issues(id INTEGER PRIMARY KEY,uid TEXT,short_id TEXT,project_id INTEGER,deleted_at TEXT)`,
				`CREATE TABLE comments(id INTEGER PRIMARY KEY,uid TEXT,issue_id INTEGER,author TEXT,body TEXT,created_at TEXT,teammate TEXT,reply_to_uid TEXT,reply_kind TEXT,edited_at TEXT)`,
				`CREATE TABLE events(type TEXT,related_issue_uid TEXT,payload TEXT)`,
				`CREATE TABLE purge_log(issue_uid TEXT)`,
				`INSERT INTO projects VALUES(1,'01M40000000000000000000001','example-project',NULL)`,
				`INSERT INTO issues VALUES(1,'01M40000000000000000000002','0002',1,NULL),(2,'01M40000000000000000000003','0003',1,NULL)`,
				`INSERT INTO comments(uid,issue_id,author,body,created_at) VALUES('01M40000000000000000000004',1,'worker','Selected issue comment','2026-01-01T00:00:00Z')`,
			} {
				_, err := pool.ExecContext(ctx, query)
				require.NoError(t, err)
			}
			for index := range unrelated {
				_, err := pool.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body,created_at,reply_to_uid,reply_kind) VALUES(?,2,'worker',?,'2026-01-01T00:00:00Z',?,'reply')`,
					fmt.Sprintf("01M5%022d", index), strings.Repeat("x", 65536), fmt.Sprintf("01M6%022d", index))
				require.NoError(t, err)
			}
			tx, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			for _, reference := range []*CommentGraphReference{nil, {UID: "01M40000000000000000000004"}} {
				*work = referenceWork{}
				graph, err := ReadCommentGraphTx(ctx, tx, CommentGraphQuery{ProjectID: 1, SelectedIssueID: 1, Reference: reference},
					func(int) string { return "?" },
					func(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) },
					`json_extract(e.payload,'$.reply_to_uid')`, "NULL")
				require.NoError(t, err)
				require.Equal(t, 1, len(graph.Comments))
				require.Equal(t, "01M40000000000000000000004", graph.Comments[0].Comment.UID)
				if reference == nil {
					require.Equal(t, "Selected issue comment", graph.Comments[0].Comment.Body)
					require.Len(t, graph.CommentUIDsByProject[1], unrelated+1)
					require.Equal(t, 22, work.bodyBytes, "unrelated bodies must not be consumed and discarded")
					require.LessOrEqual(t, work.queries, 2, "unrelated pending targets must not trigger endpoint lookups")
				} else {
					require.Zero(t, work.bodyBytes)
					require.Equal(t, 1, work.rows, "full UID resolution must not read unrelated identities")
					require.Equal(t, 1, work.queries)
				}
				t.Logf("unrelated=%d reference=%t queries=%d rows=%d body_bytes=%d", unrelated, reference != nil, work.queries, work.rows, work.bodyBytes)
			}
		})
	}
}

func TestReadCommentGraphPendingTargetWork(t *testing.T) {
	var singleReplyQueries int
	for _, replies := range []int{1, 32} {
		ctx := t.Context()
		work := &referenceWork{}
		pool := sql.OpenDB(referenceConnector{work})
		pool.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = pool.Close() })
		for _, query := range []string{
			`CREATE TABLE projects(id INTEGER PRIMARY KEY,uid TEXT,name TEXT,deleted_at TEXT)`,
			`CREATE TABLE issues(id INTEGER PRIMARY KEY,uid TEXT,short_id TEXT,project_id INTEGER,deleted_at TEXT)`,
			`CREATE TABLE comments(id INTEGER PRIMARY KEY,uid TEXT,issue_id INTEGER,author TEXT,body TEXT,created_at TEXT,teammate TEXT,reply_to_uid TEXT,reply_kind TEXT,edited_at TEXT)`,
			`CREATE TABLE events(type TEXT,related_issue_uid TEXT,payload TEXT)`,
			`CREATE TABLE purge_log(issue_uid TEXT)`,
			`INSERT INTO projects VALUES(1,'01M40000000000000000000001','example-project',NULL)`,
			`INSERT INTO issues VALUES(1,'01M40000000000000000000002','0002',1,NULL)`,
		} {
			_, err := pool.ExecContext(ctx, query)
			require.NoError(t, err)
		}
		const targetUID = "01M40000000000000000000003"
		for index := range replies {
			_, err := pool.ExecContext(ctx,
				`INSERT INTO comments(uid,issue_id,author,body,created_at,reply_to_uid,reply_kind) VALUES(?,1,'worker','Pending reply','2026-01-01T00:00:00Z',?,'reply')`,
				fmt.Sprintf("01M5%022d", index), targetUID)
			require.NoError(t, err)
		}
		tx, err := pool.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		*work = referenceWork{}
		graph, err := ReadCommentGraphTx(ctx, tx, CommentGraphQuery{ProjectID: 1},
			func(int) string { return "?" },
			func(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) },
			`json_extract(e.payload,'$.reply_to_uid')`, "NULL")
		require.NoError(t, err)
		require.Len(t, graph.Comments, replies)
		require.Equal(t, map[string]CommentGraphTarget{targetUID: {Status: "pending"}}, graph.Targets)
		t.Logf("replies=%d queries=%d", replies, work.queries)
		if replies == 1 {
			singleReplyQueries = work.queries
		} else {
			require.Equal(t, singleReplyQueries, work.queries, "replies to the same target must reuse its snapshot evidence")
		}
	}
}
