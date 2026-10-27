package sqlitestore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A parent with hundreds of children must load within the client's deadline
// even when the authorized projects contain tens of thousands of issues.
func TestRelationshipsByIssuesAuthorizedAtScale(t *testing.T) {
	t.Parallel()
	store := openUIStore(t)
	projects := make([]string, 34)
	for i := range projects {
		projects[i] = createUIProject(t, store, fmt.Sprintf("project-%02d", i+1)).UID
	}
	_, err := store.ExecContext(t.Context(), `WITH RECURSIVE sequence(n) AS (
		VALUES(1) UNION ALL SELECT n+1 FROM sequence WHERE n<61202
	)
	INSERT INTO issues (id, uid, project_id, short_id, title, author)
	SELECT n, printf('%026d', n),
	       (SELECT id FROM projects WHERE name=printf('project-%02d', (n-1)%34+1)),
	       printf('%06d', n), 'Issue ' || n, 'user-a'
	FROM sequence`)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), `INSERT INTO links
		(from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		SELECT child.id, parent.id, child.uid, parent.uid, 'parent', 'user-a'
		FROM issues child JOIN issues parent ON parent.id=1
		WHERE child.id BETWEEN 2 AND 282`)
	require.NoError(t, err)
	ids := make([]int64, 281)
	for i := range ids {
		ids[i] = int64(i + 2)
	}
	ctx, cancel := context.WithTimeout(db.WithAuthorizedProjects(t.Context(), projects), 5*time.Second)
	defer cancel()
	got, err := store.RelationshipsByIssues(ctx, ids)
	require.NoError(t, err)
	require.Len(t, got, 281)
	for _, id := range ids {
		require.NotNil(t, got[id].ParentIssueID, "child %d", id)
		require.Equal(t, int64(1), *got[id].ParentIssueID, "child %d", id)
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
