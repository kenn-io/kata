package pgstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A link is visible only when both of its endpoints are in an authorized
// project, whichever alias the query gives the links table.
func TestAuthorizedLinkPredicateBoundsBothEndpoints(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	visible := createUIProject(t, store, "spoke-project")
	hidden := createUIProject(t, store, "hub-project")
	create := func(projectID int64, title string) db.Issue {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "user-a"})
		require.NoError(t, err)
		return issue
	}
	a, b, h := create(visible.ID, "a"), create(visible.ID, "b"), create(hidden.ID, "h")
	for _, link := range []db.CreateLinkParams{
		{FromIssueID: a.ID, ToIssueID: b.ID, Type: "related", Author: "user-a"},
		{FromIssueID: a.ID, ToIssueID: h.ID, Type: "blocks", Author: "user-a"},
		{FromIssueID: h.ID, ToIssueID: b.ID, Type: "blocks", Author: "user-a"},
	} {
		_, err := store.CreateLink(ctx, link)
		require.NoError(t, err)
	}
	count := func(ctx context.Context, from, prefix string) int {
		args := []any{"ignored leading argument"}
		var n int
		require.NoError(t, store.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+from+` WHERE $1::text IS NOT NULL AND `+authorizedLinkPredicate(ctx, prefix, &args), args...).Scan(&n))
		return n
	}
	for _, shape := range []struct{ from, prefix string }{{"links l", "l."}, {"links relation", "relation."}, {"links", ""}} {
		require.Equal(t, 3, count(ctx, shape.from, shape.prefix), "unrestricted %q", shape.prefix)
		require.Equal(t, 1, count(db.WithAuthorizedProjects(ctx, []string{visible.UID}), shape.from, shape.prefix), "visible only %q", shape.prefix)
		require.Equal(t, 0, count(db.WithAuthorizedProjects(ctx, []string{hidden.UID}), shape.from, shape.prefix), "hidden only %q", shape.prefix)
		require.Equal(t, 0, count(db.WithAuthorizedProjects(ctx, []string{}), shape.from, shape.prefix), "no projects %q", shape.prefix)
		require.Equal(t, 3, count(db.WithAuthorizedProjects(ctx, []string{visible.UID, hidden.UID}), shape.from, shape.prefix), "both %q", shape.prefix)
	}
}
