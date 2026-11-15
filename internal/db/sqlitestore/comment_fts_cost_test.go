package sqlitestore_test

import (
	"context"
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

func TestSearchFTS_CommonWordPageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	p, err := d.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	_, err = raw.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<20000)
 INSERT INTO issues(uid,project_id,short_id,title,author)
 SELECT printf('%026d',x),?,printf('%08d',x),'common','tester' FROM n`, p.ID)
	require.NoError(t, err)
	store := &sqlitestore.Store{DB: raw}
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){
		"all": store.SearchFTS, "any": store.SearchFTSAny,
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			start := time.Now()
			hits, err := search(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "common", Limit: 20})
			elapsed := time.Since(start)
			require.NoError(t, err)
			require.Len(t, hits, 20)
			for n, hit := range hits {
				require.Equal(t, int64(n+1), hit.Issue.ID)
				require.Equal(t, []string{"title"}, hit.MatchedIn)
			}
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			count := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("20k issues / common / limit 20: %d page fetches, %.3f ms", count, float64(elapsed)/float64(time.Millisecond))
			// Eligibility reads each matching issue. Leave 1k fetches beyond
			// those 40k reads for the indexes and the 20 result projections.
			require.LessOrEqual(t, count, 41_000, "only the requested page needs issue and project projections")
		})
	}
}

func TestSearchFTS_PageBudget(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		comments                  int
		otherProject, interleaved bool
		query                     string
		want, budget              int
	}{
		{"sparse same-project", 100_000, false, false, "needle", 1, 512},
		{"dense other-project", 30_000, true, false, "login", 1, 4096},
		{"interleaved", 30_000, true, true, "login browser safari timeout", 1, 16_384},
		{"absent word", 0, false, false, "absentword", 0, 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, path := openTestDBWithPath(t)
			ctx := t.Context()
			p, err := d.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			issue := createTesterIssueWithBody(ctx, t, d, p.ID, "login browser", "safari timeout")
			commentProject := p.ID
			if tc.otherProject {
				other, err := d.CreateProject(ctx, "other-project")
				require.NoError(t, err)
				commentProject = other.ID
			}
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = raw.Close() })
			raw.SetMaxOpenConns(1)
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			// Many issues also catch queries that probe every issue for absent words.
			_, err = tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT printf('%026d',x),?,printf('%08d',x),'ordinary task','tester' FROM n`, commentProject)
			require.NoError(t, err)
			insertComment := prepareSeed(t, tx, `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,?,'tester',?)`)
			for n := range tc.comments {
				body := "login browser safari timeout"
				id := issue.ID + 1 + int64(n%300)
				if tc.name == "sparse same-project" {
					body = "ordinary text"
					if n == 0 {
						body = "needle"
					}
				}
				if tc.interleaved && (n == 0 || n == tc.comments-1) {
					id = issue.ID
				}
				_, err = insertComment.ExecContext(ctx, fmt.Sprintf("%026d", n+1), id, body)
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			store := &sqlitestore.Store{DB: raw}
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			start := time.Now()
			hits, err := store.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: tc.query})
			elapsed := time.Since(start)
			require.NoError(t, err)
			require.Len(t, hits, tc.want)
			for _, hit := range hits {
				require.Equal(t, p.ID, hit.Issue.ProjectID)
			}
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			count := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("%d comments: %d page fetches, %.3f ms", tc.comments, count, float64(elapsed)/float64(time.Millisecond))
			require.LessOrEqual(t, count, tc.budget, "search must follow matching postings")
		})
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
	pages := func(reset bool) int { return sqlitePageFetches(t, conn, reset) }
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

func sqlitePageFetches(t *testing.T, conn *sql.Conn, reset bool) int {
	t.Helper()
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
