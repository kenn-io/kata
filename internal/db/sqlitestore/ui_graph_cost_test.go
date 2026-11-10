package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestReadUIGraphLinksLargeIssueSet(t *testing.T) {
	store, path := openTestDBWithPath(t)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), `
		WITH RECURSIVE filler(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM filler WHERE n < 16386
		)
		INSERT INTO issues (uid, project_id, short_id, title, author)
		SELECT printf('%026d', n), ?, printf('%08d', n), 'Filler issue', 'user-a'
		FROM filler`, project.ID)
	require.NoError(t, err)
	wantIDs := []int64{}
	for index, endpoints := range [][2]int64{{2, 1}, {1, 3}, {1, 16386}, {16386, 1}, {16386, 16385}} {
		link, err := store.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: endpoints[0], ToIssueID: endpoints[1], Type: "blocks", Author: "user-a",
		})
		require.NoError(t, err)
		if index < 2 {
			wantIDs = append(wantIDs, link.ID)
		}
	}
	rows, err := store.QueryContext(t.Context(), `
		INSERT INTO links (from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		SELECT 1, id, printf('%026d', 1), uid, 'blocks', 'user-a'
		FROM issues WHERE id BETWEEN 4 AND 16384
		RETURNING id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		wantIDs = append(wantIDs, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	// Keep links with one or both endpoints outside the visible snapshot.
	for _, id := range []int64{16385, 16386} {
		_, _, _, err := store.SoftDeleteIssue(t.Context(), id, "user-a")
		require.NoError(t, err)
	}
	// Use one raw connection so page counts cover the whole snapshot.
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	reader := &sqlitestore.Store{DB: raw}
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	sqlitePageFetches(t, conn, true)
	require.NoError(t, conn.Close())

	snapshot, err := reader.ReadUISnapshot(t.Context(), db.UISnapshotQuery{
		View: "all-open", IncludeGraph: true, SelectedIssueUID: fmt.Sprintf("%026d", 1),
	})
	require.NoError(t, err)
	conn, err = raw.Conn(t.Context())
	require.NoError(t, err)
	pages := sqlitePageFetches(t, conn, false)
	require.NoError(t, conn.Close())
	t.Logf("16k issues and links: %d snapshot page fetches", pages)
	// Joined details use about 302k fetches. Either per-link detail loop
	// raises that above 430k; quadratic ID-pair probes cost much more.
	require.LessOrEqual(t, pages, 400_000, "snapshot link details must be read in batches")
	require.Len(t, snapshot.Issues, 16384)
	require.Len(t, snapshot.GraphIssues, 16384)
	for _, links := range [][]db.UILink{snapshot.CollectionLinks, snapshot.SelectedLinks, snapshot.GraphLinks} {
		gotIDs := make([]int64, 0, len(links))
		for _, link := range links {
			gotIDs = append(gotIDs, link.ID)
		}
		require.Equal(t, wantIDs, gotIDs)
		require.Equal(t, "example-project#00000002", links[0].FromQualifiedID)
		require.Equal(t, "example-project#00000001", links[0].ToQualifiedID)
		require.Equal(t, "open", links[0].FromStatus)
		require.Equal(t, "open", links[0].ToStatus)
	}
}
