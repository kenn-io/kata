package db

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelectedCommentGraphWorkIgnoresUnrelatedBodies(t *testing.T) {
	for _, unrelated := range []int{0, 256} {
		var singleCommentSuffixArgs int
		for _, selected := range []int{1, 32} {
			t.Run(fmt.Sprintf("unrelated=%d/selected=%d", unrelated, selected), func(t *testing.T) {
				ctx := t.Context()
				work := &referenceWork{}
				pool := sql.OpenDB(referenceConnector{work})
				pool.SetMaxOpenConns(1)
				t.Cleanup(func() { _ = pool.Close() })
				for _, query := range []string{
					`CREATE TABLE projects(id INTEGER PRIMARY KEY,uid TEXT,name TEXT,deleted_at TEXT)`,
					`CREATE TABLE issues(id INTEGER PRIMARY KEY,uid TEXT,short_id TEXT,project_id INTEGER,deleted_at TEXT)`,
					`CREATE TABLE comments(id INTEGER PRIMARY KEY,uid TEXT,issue_id INTEGER,author TEXT,body TEXT,created_at TEXT,teammate TEXT,reply_to_uid TEXT,reply_kind TEXT,edited_at TEXT)`,
					`CREATE TABLE events(uid TEXT,origin_instance_uid TEXT,project_id INTEGER,type TEXT,issue_uid TEXT,hlc_physical_ms INTEGER,hlc_counter INTEGER,related_issue_uid TEXT,payload TEXT)`,
					`CREATE TABLE purge_log(issue_uid TEXT)`,
					`INSERT INTO projects VALUES(1,'01M40000000000000000000001','example-project',NULL)`,
					`INSERT INTO issues VALUES(1,'01M40000000000000000000002','0002',1,NULL),(2,'01M40000000000000000000003','0003',1,NULL)`,
					`INSERT INTO comments(uid,issue_id,author,body,created_at) VALUES('01M40000000000000000000004',1,'worker','Selected issue comment','2026-01-01T00:00:00Z')`,
				} {
					_, err := pool.ExecContext(ctx, query)
					require.NoError(t, err)
				}
				expected := []string{"01M40000000000000000000004"}
				for index := 1; index < selected; index++ {
					uid := fmt.Sprintf("01M7%022d", index+1000)
					_, err := pool.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body,created_at) VALUES(?,1,'worker','Selected issue comment','2026-01-01T00:00:00Z')`, uid)
					require.NoError(t, err)
					expected = append(expected, uid)
				}
				if unrelated > 4 {
					expected = append(expected, "01M50000000000000000000004")
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
					var suffixArgs int
					work.capture = func(statement string, args []driver.NamedValue) {
						if strings.HasPrefix(statement, "SELECT c.uid FROM") {
							suffixArgs = len(args)
						}
					}
					graph, err := ReadCommentGraphTx(ctx, tx, CommentGraphQuery{ProjectID: 1, SelectedIssueID: 1, Reference: reference},
						func(int) string { return "?" },
						func(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) },
						`json_extract(e.payload,'$.reply_to_uid')`, "NULL", CommentDisplaySQLite)
					require.NoError(t, err)
					require.Equal(t, "01M40000000000000000000004", graph.Comments[0].Comment.UID)
					if reference == nil {
						require.Equal(t, "Selected issue comment", graph.Comments[0].Comment.Body)
						require.Len(t, graph.Comments, selected)
						if selected == 1 {
							require.Positive(t, suffixArgs, "the suffix lookup must run")
							singleCommentSuffixArgs = suffixArgs
						} else {
							require.Equal(t, singleCommentSuffixArgs, suffixArgs, "suffix lookup arguments stay fixed as selected comments grow")
						}
						require.ElementsMatch(t, expected, graph.CommentUIDsByProject[1])
						require.LessOrEqual(t, work.rows, 2*selected+1, "handle rows stay bounded as unrelated comments grow")
						require.Equal(t, 22*selected, work.bodyBytes, "unrelated bodies must not be consumed and discarded")
						require.LessOrEqual(t, work.queries, 3, "one contributor history read is allowed; unrelated pending targets must not trigger endpoint lookups")
					} else {
						require.Len(t, graph.Comments, 1)
						require.Zero(t, work.bodyBytes)
						require.Equal(t, 1, work.rows, "full UID resolution must not read unrelated identities")
						require.Equal(t, 1, work.queries)
					}
					t.Logf("unrelated=%d reference=%t queries=%d rows=%d body_bytes=%d", unrelated, reference != nil, work.queries, work.rows, work.bodyBytes)
				}
			})
		}
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
			`CREATE TABLE events(uid TEXT,origin_instance_uid TEXT,project_id INTEGER,type TEXT,issue_uid TEXT,hlc_physical_ms INTEGER,hlc_counter INTEGER,related_issue_uid TEXT,payload TEXT)`,
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
		graph, err := ReadCommentGraphTx(ctx, tx, CommentGraphQuery{ProjectID: 1, SelectedIssueID: 1},
			func(int) string { return "?" },
			func(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) },
			`json_extract(e.payload,'$.reply_to_uid')`, "NULL", CommentDisplaySQLite)
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

// Comment graph reads must use lookup indexes without ANALYZE, as production does.
func TestSelectedCommentGraphUsesLookupIndexes(t *testing.T) {
	ctx := t.Context()
	work := &referenceWork{}
	pool := sql.OpenDB(referenceConnector{work})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	schema, err := os.ReadFile("sqlitestore/schema.sql")
	require.NoError(t, err)
	_, err = pool.ExecContext(ctx, string(schema))
	require.NoError(t, err)
	const targetUID = "01m40000000000000000abcdef"
	for _, statement := range []string{
		`INSERT INTO projects(id,uid,name) VALUES(1,'01M40000000000000000000001','example-project')`,
		`INSERT INTO issues(id,uid,short_id,project_id,title,author) VALUES(1,'01M40000000000000000000002','0002',1,'Selected','worker')`,
		`INSERT INTO comments(uid,issue_id,author,body) VALUES('01M40000000000000000000004',1,'worker','Selected comment')`,
		`INSERT INTO comments(uid,issue_id,author,body) VALUES('01M40000000000000000000005',1,'worker','Second comment'),('01M40000000000000000000006',1,'worker','Third comment')`,
		`WITH RECURSIVE n(x) AS (VALUES(1000) UNION ALL SELECT x+1 FROM n WHERE x<1999) INSERT INTO issues(id,uid,short_id,project_id,title,author) SELECT x,printf('01M4%022d',x),printf('%04d',x),1,'Other','worker' FROM n`,
		`INSERT INTO comments(uid,issue_id,author,body) VALUES('01m40000000000000000abcdef',1,'worker','Target')`,
		`INSERT INTO comments(uid,issue_id,author,body) SELECT printf('01M5%022d',id),id,'worker','Unrelated' FROM issues WHERE id>=1000`,
	} {
		_, err := pool.ExecContext(ctx, statement)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		name    string
		query   CommentGraphQuery
		indexes []string
	}{
		{
			name:    "selected issue",
			query:   CommentGraphQuery{ProjectID: 1, SelectedIssueID: 1},
			indexes: []string{"idx_comments_reply_to", "idx_comments_uid_handle"},
		},
		{
			name:    "full UID reference",
			query:   CommentGraphQuery{ProjectID: 1, Reference: &CommentGraphReference{UID: strings.ToUpper(targetUID)}},
			indexes: []string{"idx_comments_uid_handle"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			var statements []string
			var arguments [][]any
			work.capture = func(statement string, args []driver.NamedValue) {
				// Contributor-history reads have their own index-plan test.
				if !strings.Contains(statement, "comments c") {
					return
				}
				statements = append(statements, statement)
				values := make([]any, len(args))
				for index, arg := range args {
					values[index] = arg.Value
				}
				arguments = append(arguments, values)
			}
			t.Cleanup(func() { work.capture = nil })
			tx, err := pool.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			graph, err := ReadCommentGraphTx(ctx, tx, tc.query,
				func(int) string { return "?" },
				func(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) },
				`json_extract(e.payload,'$.reply_to_uid')`, "NULL", CommentDisplaySQLite)
			require.NoError(t, err)
			if tc.query.Reference != nil {
				require.Len(t, graph.Comments, 1)
				require.Equal(t, targetUID, graph.Comments[0].Comment.UID)
			}
			require.NoError(t, tx.Rollback())
			work.capture = nil
			require.Len(t, statements, len(tc.indexes))
			for index, statement := range statements {
				rows, err := pool.QueryContext(ctx, "EXPLAIN QUERY PLAN "+statement, arguments[index]...)
				require.NoError(t, err)
				var plan []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
					plan = append(plan, detail)
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				planText := strings.Join(plan, "\n")
				require.Contains(t, planText, "INDEX "+tc.indexes[index])
				if tc.query.Reference != nil {
					require.NotContains(t, planText, "idx_comments_issue")
				}
			}
		})
	}
}
