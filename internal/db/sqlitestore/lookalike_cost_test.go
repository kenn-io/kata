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
)

// Look-alike retrieval runs on issue creation and must stay cheap as comments grow.
func TestSearchFTSAny_LookalikePageBudget(t *testing.T) {
	ctx := t.Context()
	store, path := openTestDBWithPath(t)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	words := strings.Fields("login browser safari timeout session request response server client token account password reset user profile settings page button form submit redirect callback cookie header connection network retry error failure crash reload refresh expired invalid missing duplicate create update delete search issue title body comment project workspace author owner label status priority open closed task progress report review change fix support handle return result message details context window screen")
	require.Len(t, words, 68)
	query := strings.Join(words, " ")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	tx, err := raw.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for n := range 2_000 {
		_, err = tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), "login browser", "safari timeout")
		require.NoError(t, err)
	}
	for n := range 30_000 {
		start := n % (len(words) - 16)
		_, err = tx.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,(SELECT id FROM issues WHERE project_id=? ORDER BY id LIMIT 1 OFFSET ?),'tester',?)`, fmt.Sprintf("%026d", n+1), project.ID, n%2_000, strings.Join(words[start:start+16], " "))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	searchStore := &sqlitestore.Store{DB: raw}
	conn, err := raw.Conn(ctx)
	require.NoError(t, err)
	sqlitePageFetches(t, conn, true)
	require.NoError(t, conn.Close())
	start := time.Now()
	hits, err := searchStore.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: query, Limit: 20})
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Len(t, hits, 20)
	conn, err = raw.Conn(ctx)
	require.NoError(t, err)
	count := sqlitePageFetches(t, conn, false)
	require.NoError(t, conn.Close())
	t.Logf("2k issues / 30k comments / 68 words: %d page fetches, %.3f ms", count, float64(elapsed)/float64(time.Millisecond))
	require.LessOrEqual(t, count, 100_000, "look-alike retrieval must rank title and body documents only")
}

func TestSearchFTSAny_DensePostingsPageBudget(t *testing.T) {
	ctx := t.Context()
	store, path := openTestDBWithPath(t)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	common := "login browser timeout session request response server client token account password reset"
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	tx, err := raw.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for n := range 2_000 {
		_, err = tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), "shared issue", common)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	original := createTesterIssueWithBody(ctx, t, store, project.ID, "distinctive callback failure", common)
	conn, err := raw.Conn(ctx)
	require.NoError(t, err)
	sqlitePageFetches(t, conn, true)
	require.NoError(t, conn.Close())
	hits, err := (&sqlitestore.Store{DB: raw}).SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: common + " distinctive callback failure", Limit: 20})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, original.ID, hits[0].Issue.ID)
	conn, err = raw.Conn(ctx)
	require.NoError(t, err)
	count := sqlitePageFetches(t, conn, false)
	require.NoError(t, conn.Close())
	t.Logf("2k issues / 15 words with dense postings: %d page fetches", count)
	require.LessOrEqual(t, count, 20_000, "look-alike retrieval must score each issue once")
}
