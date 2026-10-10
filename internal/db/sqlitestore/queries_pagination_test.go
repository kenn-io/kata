package sqlitestore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/pagination"
)

func TestListIssuesPaginationOrdersStoredOffsetTimestampsByInstant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestDB(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)

	offset, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "offset-later", Author: "example-agent"})
	require.NoError(t, err)
	utc, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "utc-earlier", Author: "example-agent"})
	require.NoError(t, err)

	vancouver, err := time.LoadLocation("America/Vancouver")
	require.NoError(t, err)
	offsetTime := time.Date(2026, 10, 1, 9, 0, 0, 123456789, vancouver)
	utcTime := time.Date(2026, 10, 1, 15, 0, 0, 123456789, time.UTC)
	_, err = store.ExecContext(ctx, `UPDATE issues SET created_at = ? WHERE id = ?`, offsetTime, offset.ID)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `UPDATE issues SET created_at = ? WHERE id = ?`, utcTime, utc.ID)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		oldest bool
		want   []db.Issue
	}{
		{name: "ascending", oldest: true, want: []db.Issue{utc, offset}},
		{name: "descending", want: []db.Issue{offset, utc}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID, OldestFirst: tc.oldest, CreatedFirst: !tc.oldest, Limit: 1})
			require.NoError(t, err)
			require.Len(t, first, 1)
			require.Equal(t, tc.want[0].ID, first[0].ID)

			second, err := store.ListIssues(ctx, db.ListIssuesParams{
				ProjectID: project.ID, OldestFirst: tc.oldest, CreatedFirst: !tc.oldest, Limit: 1,
				After: &pagination.Position{CreatedAt: first[0].CreatedAt, ID: first[0].ID},
			})
			require.NoError(t, err)
			require.Len(t, second, 1)
			require.Equal(t, tc.want[1].ID, second[0].ID)
		})
	}
}
