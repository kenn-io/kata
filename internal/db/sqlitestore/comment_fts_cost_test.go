package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"modernc.org/sqlite"
)

func TestSearchFTS_ScopedPageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	p, err := d.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "login browser", "safari timeout")
	other, err := d.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	thread := createTesterIssueWithBody(ctx, t, d, other.ID, "ordinary task", "details")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	raw.SetMaxOpenConns(1)
	// Search uses the real store query on an unwrapped driver connection so
	// DBStatus includes every page read, including filtered-out documents.
	searchStore := &sqlitestore.Store{DB: raw}
	pages := func(reset bool) int {
		conn, err := raw.Conn(ctx)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		total := 0
		require.NoError(t, conn.Raw(func(raw any) error {
			for _, op := range []sqlite.DBStatusOp{sqlite.DBStatusCacheHit, sqlite.DBStatusCacheMiss} {
				n, _, err := raw.(sqlite.DBStatus).Status(op, reset)
				if err != nil {
					return err
				}
				total += n
			}
			return nil
		}))
		return total
	}
	baseline := 0
	for _, size := range []int{0, 30_000} {
		if size > 0 {
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			for n := range size {
				_, err = tx.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,?,?,?)`, fmt.Sprintf("%026d", n+1), thread.ID, "tester", "login browser safari timeout")
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
		}
		pages(true)
		start := time.Now()
		hits, err := searchStore.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login browser safari timeout"})
		elapsed := time.Since(start)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		require.Equal(t, issue.ID, hits[0].Issue.ID)
		count := pages(false)
		t.Logf("%d other-project comments: %d page fetches, four-term search %s", size, count, elapsed)
		if size == 0 {
			baseline = count
		} else {
			require.LessOrEqual(t, count, baseline+512, "scoped search must avoid ranking another project's comments")
		}
	}
}

// Page fetches include reads performed inside triggers, unlike returned-row counts.
func TestCommentWritesReadFixedPagesAsIssueGrows(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	p, err := d.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "ordinary task", "details")
	// The store's write-queue wrapper doesn't expose the driver's DBStatus.
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	conn, err := raw.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	pages := func(reset bool) int {
		total := 0
		require.NoError(t, conn.Raw(func(raw any) error {
			for _, op := range []sqlite.DBStatusOp{sqlite.DBStatusCacheHit, sqlite.DBStatusCacheMiss} {
				n, _, err := raw.(sqlite.DBStatus).Status(op, reset)
				if err != nil {
					return err
				}
				total += n
			}
			return nil
		}))
		return total
	}
	counts := make(map[string]int)
	for _, size := range []int{16, 256} {
		for n := range size {
			_, err = conn.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,?,?,?)`, fmt.Sprintf("%026d", size*1000+n), issue.ID, "tester", strings.Repeat("neutral text ", 170))
			require.NoError(t, err)
		}
		for _, op := range []struct {
			name, sql string
			args      []any
		}{
			{"insert", `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,?,?,?)`, []any{fmt.Sprintf("%026d", size), issue.ID, "tester", "searchable note"}},
			{"edit", `UPDATE comments SET body='replacement note' WHERE uid=?`, []any{fmt.Sprintf("%026d", size)}},
			{"delete", `DELETE FROM comments WHERE uid=?`, []any{fmt.Sprintf("%026d", size)}},
			{"title", `UPDATE issues SET title='updated task' WHERE id=?`, []any{issue.ID}},
		} {
			pages(true)
			_, err = conn.ExecContext(ctx, op.sql, op.args...)
			require.NoError(t, err)
			count := pages(false)
			t.Logf("%d comments %s: %d page fetches", size, op.name, count)
			if size == 16 {
				counts[op.name] = count
			} else {
				require.LessOrEqual(t, count, counts[op.name]+32, op.name)
			}
		}
		_, err = conn.ExecContext(ctx, `DELETE FROM comments WHERE issue_id=?`, issue.ID)
		require.NoError(t, err)
	}
}
