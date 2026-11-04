package pgstore_test

import (
	"context"
	"testing"

	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestSearchFTS_TokenizerWords(t *testing.T) {
	if testing.Short() {
		t.Skip("requires isolated PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	tagged, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "login 🧪", Author: "tester"})
	require.NoError(t, err)
	plain, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "login", Author: "tester"})
	require.NoError(t, err)
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){
		"all": store.SearchFTS, "any": store.SearchFTSAny,
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				query string
				want  []int64
			}{
				{"🧪", nil},
				{"!!!", nil},
				{"login !!!", []int64{tagged.ID, plain.ID}},
				{"login 🧪 !!!", []int64{tagged.ID, plain.ID}},
			} {
				t.Run(tc.query, func(t *testing.T) {
					hits, err := search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: tc.query})
					require.NoError(t, err)
					var ids []int64
					for _, hit := range hits {
						ids = append(ids, hit.Issue.ID)
					}
					require.ElementsMatch(t, tc.want, ids)
				})
			}
		})
	}
}

func TestSearchFTS_LiteralURL(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "http://a.example/x'y", Author: "tester"})
	require.NoError(t, err)
	for _, search := range []func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){store.SearchFTS, store.SearchFTSAny} {
		hits, err := search(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "http://a.example/x'y"})
		require.NoError(t, err)
		require.Len(t, hits, 1)
		require.Equal(t, issue.ID, hits[0].Issue.ID)
	}
}

func TestSearchKitBuildersPreserveFieldsScopeAndOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("requires isolated PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "root", Author: "example-author"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "café login", Body: "session expired", Author: "example-author",
		Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}, Labels: []string{"bug"},
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: child.ID, Body: "browser reset", Author: "example-author"})
	require.NoError(t, err)
	sibling, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "café login", Body: "session expired", Author: "example-author", Labels: []string{"bug"},
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: sibling.ID, Body: "browser reset", Author: "example-author"})
	require.NoError(t, err)
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){
		"all": store.SearchFTS, "any": store.SearchFTSAny,
	} {
		t.Run(name, func(t *testing.T) {
			for query, fields := range map[string][]string{
				"cafe": {"title"}, "session": {"body"}, "browser": {"comments"},
				"login session browser": {"title", "body", "comments"},
			} {
				hits, err := search(ctx, db.SearchFTSParams{
					ProjectID: project.ID, Query: query, Limit: 1, Status: "open", Labels: []string{"BUG"},
					IssueScope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID},
				})
				require.NoError(t, err)
				require.Len(t, hits, 1, "query %q must match the scoped child", query)
				require.Equal(t, child.UID, hits[0].Issue.UID)
				require.Equal(t, fields, hits[0].MatchedIn)
				require.Equal(t, child.Revision, hits[0].Issue.Revision)
			}
			hits, err := search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "login", Limit: 1})
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, sibling.UID, hits[0].Issue.UID, "equal-score candidates retain the descending ID tie-break before LIMIT")
			hits, err = search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "login", Limit: 1, AllowedIssueIDs: []int64{child.ID}})
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, child.UID, hits[0].Issue.UID)
			hits, err = search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "login", Limit: 1, ExcludeLabels: []string{"BUG"}})
			require.NoError(t, err)
			require.Empty(t, hits)
		})
	}
}
