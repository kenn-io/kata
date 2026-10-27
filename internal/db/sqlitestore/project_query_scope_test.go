package sqlitestore

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Authorization must check the endpoints of candidate links rather than make
// SQLite probe every combination of requested and visible issue IDs.
func TestAuthorizedLinkPredicateUsesEndpointLookups(t *testing.T) {
	t.Parallel()
	store := openUIStore(t)
	projects := make([]string, 34)
	for i := range projects {
		projects[i] = fmt.Sprintf("%026d", i+1)
	}
	ctx := db.WithAuthorizedProjects(t.Context(), projects)
	ids := make([]int64, 281)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	placeholders, parentArgs := relationshipChunkPlaceholders(ids)
	parentPredicate := authorizedLinkPredicate(ctx, "l.", &parentArgs)
	linkArgs := []any{1, 1}
	linkPredicate := authorizedLinkPredicate(ctx, "", &linkArgs)
	for _, test := range []struct {
		name, query string
		args        []any
		searches    []string
	}{
		{"parents", `SELECT l.from_issue_id, parent.id FROM links l
		 JOIN issues child ON child.id=l.from_issue_id JOIN issues parent ON parent.id=l.to_issue_id
		 WHERE l.type='parent' AND l.from_issue_id IN (` + placeholders + `) AND ` + parentPredicate + ` ORDER BY l.from_issue_id ASC`,
			parentArgs, []string{"from_issue_id=?"}},
		{"issue links", linkSelect + ` WHERE (from_issue_id=? OR to_issue_id=?) AND ` + linkPredicate + ` ORDER BY id ASC`,
			linkArgs, []string{"from_issue_id=?", "to_issue_id=?"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := store.QueryContext(ctx, `EXPLAIN QUERY PLAN `+test.query, test.args...)
			require.NoError(t, err)
			var plan []string
			var endpointLookups int
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan = append(plan, detail)
				if strings.HasPrefix(detail, "SEARCH endpoint ") && strings.Contains(detail, "USING INTEGER PRIMARY KEY") {
					endpointLookups++
				}
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			joined := strings.Join(plan, "\n")
			require.Equal(t, 2, endpointLookups, "plan: %s", joined)
			for _, search := range test.searches {
				require.Contains(t, joined, search, "plan: %s", joined)
			}
		})
	}
}

// A link is visible only when both of its endpoints are in an authorized
// project, whichever alias the query gives the links table.
func TestAuthorizedLinkPredicateBoundsBothEndpoints(t *testing.T) {
	t.Parallel()
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
		var args []any
		var n int
		require.NoError(t, store.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+from+` WHERE `+authorizedLinkPredicate(ctx, prefix, &args), args...).Scan(&n))
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
